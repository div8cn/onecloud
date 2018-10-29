package azure

import (
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/onecloud/pkg/cloudprovider"
	"yunion.io/x/onecloud/pkg/compute/models"
	"yunion.io/x/pkg/util/netutils"
)

type SClassicNetwork struct {
	wire *SClassicWire

	id            string
	Name          string
	AddressPrefix string `json:"addressPrefix,omitempty"`
}

func (self *SClassicNetwork) GetMetadata() *jsonutils.JSONDict {
	return nil
}

func (self *SClassicNetwork) GetId() string {
	return strings.ToLower(self.id)
}

func (self *SClassicNetwork) GetName() string {
	return self.Name
}

func (self *SClassicNetwork) GetGlobalId() string {
	return self.GetId()
}

func (self *SClassicNetwork) IsEmulated() bool {
	return false
}

func (self *SClassicNetwork) GetStatus() string {
	return "available"
}

func (self *SClassicNetwork) Delete() error {
	vpc := self.wire.vpc
	subnets := []SClassicNetwork{}
	for i := 0; i < len(vpc.Properties.Subnets); i++ {
		network := vpc.Properties.Subnets[i]
		if network.Name == self.Name && self.AddressPrefix == network.AddressPrefix {
			continue
		}
		subnets = append(subnets, network)
	}
	return self.wire.vpc.region.client.Update(jsonutils.Marshal(vpc), self.wire.vpc)
}

func (self *SClassicNetwork) GetGateway() string {
	pref, _ := netutils.NewIPV4Prefix(self.AddressPrefix)
	endIp := pref.Address.BroadcastAddr(pref.MaskLen) // 255
	endIp = endIp.StepDown()                          // 254
	return endIp.String()
}

func (self *SClassicNetwork) GetIWire() cloudprovider.ICloudWire {
	return self.wire
}

func (self *SClassicNetwork) GetIpEnd() string {
	pref, _ := netutils.NewIPV4Prefix(self.AddressPrefix)
	endIp := pref.Address.BroadcastAddr(pref.MaskLen) // 255
	endIp = endIp.StepDown()                          // 254
	endIp = endIp.StepDown()                          // 253
	endIp = endIp.StepDown()                          // 252
	return endIp.String()
}

func (self *SClassicNetwork) GetIpMask() int8 {
	pref, _ := netutils.NewIPV4Prefix(self.AddressPrefix)
	return pref.MaskLen
}

func (self *SClassicNetwork) GetIpStart() string {
	pref, _ := netutils.NewIPV4Prefix(self.AddressPrefix)
	startIp := pref.Address.NetAddr(pref.MaskLen) // 0
	startIp = startIp.StepUp()                    // 1
	return startIp.String()
}

func (self *SClassicNetwork) GetIsPublic() bool {
	return true
}

func (self *SClassicNetwork) GetServerType() string {
	return models.SERVER_TYPE_GUEST
}

func (self *SClassicNetwork) Refresh() error {
	err := self.wire.vpc.Refresh()
	if err != nil {
		return err
	}
	for _, network := range self.wire.vpc.Properties.Subnets {
		if network.Name == self.Name {
			self.AddressPrefix = network.AddressPrefix
			return nil
		}
	}
	return cloudprovider.ErrNotFound
}

func (self *SClassicNetwork) GetAllocTimeoutSeconds() int {
	return 120 // 2 minutes
}
