// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package policy

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"

	"yunion.io/x/onecloud/pkg/appsrv"
	"yunion.io/x/onecloud/pkg/cloudcommon/consts"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/mcclient"
	"yunion.io/x/onecloud/pkg/mcclient/auth"
	"yunion.io/x/onecloud/pkg/mcclient/modules"
	"yunion.io/x/onecloud/pkg/util/hashcache"
	"yunion.io/x/onecloud/pkg/util/rbacutils"
)

const (
	PolicyDelegation = "delegate"

	PolicyActionList    = "list"
	PolicyActionGet     = "get"
	PolicyActionUpdate  = "update"
	PolicyActionPatch   = "patch"
	PolicyActionCreate  = "create"
	PolicyActionDelete  = "delete"
	PolicyActionPerform = "perform"
)

type PolicyFetchFunc func() (map[rbacutils.TRbacScope]map[string]*rbacutils.SRbacPolicy, error)

var (
	PolicyManager        *SPolicyManager
	DefaultPolicyFetcher PolicyFetchFunc

	syncWorkerManager *appsrv.SWorkerManager
)

func init() {
	PolicyManager = &SPolicyManager{
		lock: &sync.Mutex{},
	}
	DefaultPolicyFetcher = remotePolicyFetcher

	syncWorkerManager = appsrv.NewWorkerManager("sync_policy_worker", 1, 1000, false)
}

type SPolicyManager struct {
	policies        map[rbacutils.TRbacScope]map[string]*rbacutils.SRbacPolicy
	defaultPolicies map[rbacutils.TRbacScope][]*rbacutils.SRbacPolicy
	lastSync        time.Time

	failedRetryInterval time.Duration
	refreshInterval     time.Duration

	cache *hashcache.Cache // policy cache

	lock *sync.Mutex
}

func parseJsonPolicy(obj jsonutils.JSONObject) (string, *rbacutils.SRbacPolicy, error) {
	typeStr, err := obj.GetString("type")
	if err != nil {
		return "", nil, errors.Wrap(err, "missing type")
	}
	domainId, err := obj.GetString("domain_id")
	if err != nil {
		return "", nil, errors.Wrap(err, "missing domain_id")
	}

	blob, err := obj.Get("policy")
	if err != nil {
		log.Errorf("get blob error %s", err)
		return "", nil, errors.Wrap(err, "json.Get")
	}

	policy := rbacutils.SRbacPolicy{}
	err = policy.Decode(blob)
	if err != nil {
		log.Errorf("policy decode error %s", err)
		return "", nil, errors.Wrap(err, "policy.Decode")
	}

	policy.DomainId = domainId

	return typeStr, &policy, nil
}

func remotePolicyFetcher() (map[rbacutils.TRbacScope]map[string]*rbacutils.SRbacPolicy, error) {
	s := auth.GetAdminSession(context.Background(), consts.GetRegion(), "v1")

	policies := make(map[rbacutils.TRbacScope]map[string]*rbacutils.SRbacPolicy)

	offset := 0
	for {
		params := jsonutils.NewDict()
		params.Add(jsonutils.NewInt(2048), "limit")
		params.Add(jsonutils.NewInt(int64(offset)), "offset")
		params.Add(jsonutils.JSONTrue, "admin")
		result, err := modules.Policies.List(s, params)
		if err != nil {
			return nil, errors.Wrap(err, "modules.Policies.List")
		}

		for i := 0; i < len(result.Data); i += 1 {
			typeStr, policy, err := parseJsonPolicy(result.Data[i])
			if err != nil {
				log.Errorf("error parse policty %s", err)
				continue
			}

			if _, ok := policies[policy.Scope]; !ok {
				policies[policy.Scope] = make(map[string]*rbacutils.SRbacPolicy)
			}
			policies[policy.Scope][typeStr] = policy
		}

		offset += len(result.Data)
		if offset >= result.Total {
			break
		}
	}
	return policies, nil
}

func (manager *SPolicyManager) start(refreshInterval time.Duration, retryInterval time.Duration) {
	log.Infof("PolicyManager start to fetch policies ...")
	manager.refreshInterval = refreshInterval
	manager.failedRetryInterval = retryInterval
	if len(defaultPolicies) > 0 {
		manager.defaultPolicies = make(map[rbacutils.TRbacScope][]*rbacutils.SRbacPolicy)
		for _, policy := range defaultPolicies {
			if _, ok := manager.defaultPolicies[policy.Scope]; !ok {
				manager.defaultPolicies[policy.Scope] = make([]*rbacutils.SRbacPolicy, 0)
			}
			manager.defaultPolicies[policy.Scope] = append(manager.defaultPolicies[policy.Scope], &policy)
		}
	}

	manager.cache = hashcache.NewCache(2048, manager.refreshInterval/2)
	err := manager.doSync()
	if err != nil {
		log.Errorf("doSync error %s", err)
		return
	}

	manager.SyncOnce()
}

func (manager *SPolicyManager) SyncOnce() {
	syncWorkerManager.Run(manager.sync, nil, nil)
}

func (manager *SPolicyManager) doSync() error {
	policies, err := DefaultPolicyFetcher()
	if err != nil {
		// log.Errorf("sync rbac policy failed: %s", err)
		return errors.Wrap(err, "DefaultPolicyFetcher")
	}

	manager.lock.Lock()
	defer manager.lock.Unlock()

	manager.policies = policies

	manager.lastSync = time.Now()
	manager.cache.Invalidate()

	return nil
}

func (manager *SPolicyManager) sync() {
	err := manager.doSync()
	var interval time.Duration
	if err != nil {
		interval = manager.failedRetryInterval
	} else {
		interval = manager.refreshInterval
	}
	time.AfterFunc(interval, manager.SyncOnce)
}

func queryKey(scope rbacutils.TRbacScope, userCred mcclient.TokenCredential, service string, resource string, action string, extra ...string) string {
	queryKeys := []string{string(scope)}
	queryKeys = append(queryKeys, userCred.GetProjectId(), userCred.GetDomainId(), userCred.GetUserId())
	roles := userCred.GetRoles()
	if len(roles) > 0 {
		sort.Strings(roles)
	}
	queryKeys = append(queryKeys, strings.Join(roles, ":"))
	if rbacutils.WILD_MATCH == service || len(service) == 0 {
		service = rbacutils.WILD_MATCH
	}
	queryKeys = append(queryKeys, service)
	if rbacutils.WILD_MATCH == resource || len(resource) == 0 {
		resource = rbacutils.WILD_MATCH
	}
	queryKeys = append(queryKeys, resource)
	if rbacutils.WILD_MATCH == action || len(action) == 0 {
		action = rbacutils.WILD_MATCH
	}
	queryKeys = append(queryKeys, action)
	if len(extra) > 0 {
		queryKeys = append(queryKeys, extra...)
	}
	return strings.Join(queryKeys, "-")
}

func (manager *SPolicyManager) AllowScope(userCred mcclient.TokenCredential, service string, resource string, action string, extra ...string) rbacutils.TRbacScope {
	for _, scope := range []rbacutils.TRbacScope{
		rbacutils.ScopeSystem,
		rbacutils.ScopeDomain,
		rbacutils.ScopeProject,
		rbacutils.ScopeUser,
	} {
		result := manager.Allow(scope, userCred, service, resource, action, extra...)
		if result == rbacutils.Allow {
			return scope
		}
	}
	return rbacutils.ScopeNone
}

func (manager *SPolicyManager) Allow(scope rbacutils.TRbacScope, userCred mcclient.TokenCredential, service string, resource string, action string, extra ...string) rbacutils.TRbacResult {
	if manager.cache != nil && userCred != nil {
		key := queryKey(scope, userCred, service, resource, action, extra...)
		val := manager.cache.Get(key)
		if val != nil {
			return val.(rbacutils.TRbacResult)
		}
		result := manager.allowWithoutCache(scope, userCred, service, resource, action, extra...)
		manager.cache.Set(key, result)
		return result
	} else {
		return manager.allowWithoutCache(scope, userCred, service, resource, action, extra...)
	}
}

func (manager *SPolicyManager) findPolicyByName(scope rbacutils.TRbacScope, name string) *rbacutils.SRbacPolicy {
	if policies, ok := manager.policies[scope]; ok {
		if p, ok := policies[name]; ok {
			return p
		}
	}
	return nil
}

func (manager *SPolicyManager) allowWithoutCache(scope rbacutils.TRbacScope, userCred mcclient.TokenCredential, service string, resource string, action string, extra ...string) rbacutils.TRbacResult {
	// var policies map[string]*rbacutils.SRbacPolicy
	policies, ok := manager.policies[scope]
	if !ok {
		log.Warningf("no policies fetched for scope %s", scope)
		// return rbacutils.Deny
	}
	findMatchRule := false
	findMatchPolicy := false
	currentPriv := rbacutils.Deny
	for _, p := range policies {
		if !p.Match(userCred) {
			continue
		}
		findMatchPolicy = true
		rule := p.GetMatchRule(service, resource, action, extra...)
		if rule != nil {
			findMatchRule = true
			if currentPriv.StricterThan(rule.Result) {
				currentPriv = rule.Result
			}
		}
	}
	if !findMatchPolicy {
		currentPriv = rbacutils.Deny
	} else if !findMatchRule {
		currentPriv = rbacutils.Allow
	}
	if currentPriv == rbacutils.Allow {
		return currentPriv
	}
	// try default policies
	defaultPolicies, ok := manager.defaultPolicies[scope]
	if !ok {
		return currentPriv
	}
	for _, p := range defaultPolicies {
		if !p.Match(userCred) {
			continue
		}
		rule := p.GetMatchRule(service, resource, action, extra...)
		if rule != nil && currentPriv.StricterThan(rule.Result) {
			currentPriv = rule.Result
		}
	}
	if consts.IsRbacDebug() {
		log.Debugf("[RBAC: %s] %s %s %s %#v permission %s userCred: %s", scope, service, resource, action, extra, currentPriv, userCred)
	}
	return currentPriv
}

func (manager *SPolicyManager) explainPolicy(userCred mcclient.TokenCredential, policyReq jsonutils.JSONObject, name string) ([]string, rbacutils.TRbacResult, error) {
	scope, request, result, err := manager.explainPolicyInternal(userCred, policyReq, name)
	if err != nil {
		return request, result, err
	}
	if result == rbacutils.Allow {
		if scope == rbacutils.ScopeProject && !isProjectResource(request[0], request[1]) {
			result = rbacutils.Deny
		} else if scope == rbacutils.ScopeDomain && isSystemResource(request[0], request[1]) {
			result = rbacutils.Deny
		}
	}
	return request, result, err
}

func (manager *SPolicyManager) explainPolicyInternal(userCred mcclient.TokenCredential, policyReq jsonutils.JSONObject, name string) (rbacutils.TRbacScope, []string, rbacutils.TRbacResult, error) {
	policySeq, err := policyReq.GetArray()
	if err != nil {
		return rbacutils.ScopeSystem, nil, rbacutils.Deny, httperrors.NewInputParameterError("invalid format")
	}
	service := rbacutils.WILD_MATCH
	resource := rbacutils.WILD_MATCH
	action := rbacutils.WILD_MATCH
	extra := make([]string, 0)
	if len(policySeq) > 1 {
		service, _ = policySeq[1].GetString()
	}
	if len(policySeq) > 2 {
		resource, _ = policySeq[2].GetString()
	}
	if len(policySeq) > 3 {
		action, _ = policySeq[3].GetString()
	}
	if len(policySeq) > 4 {
		for i := 4; i < len(policySeq); i += 1 {
			ev, _ := policySeq[i].GetString()
			extra = append(extra, ev)
		}
	}

	reqStrs := []string{service, resource, action}
	if len(extra) > 0 {
		reqStrs = append(reqStrs, extra...)
	}

	scopeStr, _ := policySeq[0].GetString()
	scope := rbacutils.String2Scope(scopeStr)
	if !consts.IsRbacEnabled() {
		if scope == rbacutils.ScopeProject || (scope == rbacutils.ScopeSystem && userCred.HasSystemAdminPrivilege()) {
			return scope, reqStrs, rbacutils.Allow, nil
		} else {
			return scope, reqStrs, rbacutils.Deny, httperrors.NewForbiddenError("operation not allowed")
		}
	}

	if len(name) == 0 {
		return scope, reqStrs, manager.allowWithoutCache(scope, userCred, service, resource, action, extra...), nil
	}

	policy := manager.findPolicyByName(scope, name)
	if policy == nil {
		return scope, reqStrs, rbacutils.Deny, httperrors.NewNotFoundError("policy %s not found", name)
	}

	rule := policy.GetMatchRule(service, resource, action, extra...)
	result := rbacutils.Deny
	if rule != nil {
		result = rule.Result
	}
	return scope, reqStrs, result, nil
}

func (manager *SPolicyManager) ExplainRpc(userCred mcclient.TokenCredential, params jsonutils.JSONObject, name string) (jsonutils.JSONObject, error) {
	paramDict, err := params.GetMap()
	if err != nil {
		return nil, httperrors.NewInputParameterError("invalid input format")
	}
	ret := jsonutils.NewDict()
	for key, policyReq := range paramDict {
		reqStrs, result, err := manager.explainPolicy(userCred, policyReq, name)
		if err != nil {
			return nil, err
		}
		reqStrs = append(reqStrs, string(result))
		ret.Add(jsonutils.NewStringArray(reqStrs), key)
	}
	return ret, nil
}

func (manager *SPolicyManager) IsScopeCapable(userCred mcclient.TokenCredential, scope rbacutils.TRbacScope) bool {
	if !consts.IsRbacEnabled() {
		if userCred.HasSystemAdminPrivilege() {
			return true
		}
		if scope == rbacutils.ScopeProject {
			return true
		}
		return false
	}

	if policies, ok := manager.policies[scope]; ok {
		for _, p := range policies {
			if p.Match(userCred) {
				return true
			}
		}
	}
	return false
}

func (manager *SPolicyManager) MatchedPolicies(scope rbacutils.TRbacScope, userCred mcclient.TokenCredential) []string {
	ret := make([]string, 0)
	policies, ok := manager.policies[scope]
	if !ok {
		return ret
	}
	for k, p := range policies {
		if p.Match(userCred) {
			ret = append(ret, k)
		}
	}
	return ret
}
