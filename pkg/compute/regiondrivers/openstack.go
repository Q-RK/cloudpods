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

package regiondrivers

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/util/secrules"

	api "yunion.io/x/onecloud/pkg/apis/compute"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/taskman"
	"yunion.io/x/onecloud/pkg/compute/models"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/mcclient"
	"yunion.io/x/onecloud/pkg/util/rbacutils"
)

type SOpenStackRegionDriver struct {
	SManagedVirtualizationRegionDriver
}

func init() {
	driver := SOpenStackRegionDriver{}
	models.RegisterRegionDriver(&driver)
}

func (self *SOpenStackRegionDriver) IsAllowSecurityGroupNameRepeat() bool {
	return true
}

func (self *SOpenStackRegionDriver) GenerateSecurityGroupName(name string) string {
	if strings.ToLower(name) == "default" {
		return "DefaultGroup"
	}
	return name
}

func (self *SOpenStackRegionDriver) GetDefaultSecurityGroupInRule() cloudprovider.SecurityRule {
	return cloudprovider.SecurityRule{SecurityRule: *secrules.MustParseSecurityRule("in:deny any")}
}

func (self *SOpenStackRegionDriver) GetDefaultSecurityGroupOutRule() cloudprovider.SecurityRule {
	return cloudprovider.SecurityRule{SecurityRule: *secrules.MustParseSecurityRule("out:deny any")}
}

func (self *SOpenStackRegionDriver) GetSecurityGroupRuleMaxPriority() int {
	return 0
}

func (self *SOpenStackRegionDriver) GetSecurityGroupRuleMinPriority() int {
	return 0
}

func (self *SOpenStackRegionDriver) IsOnlySupportAllowRules() bool {
	return true
}

func (self *SOpenStackRegionDriver) GetSecurityGroupPublicScope(service string) rbacutils.TRbacScope {
	return rbacutils.ScopeProject
}

func (self *SOpenStackRegionDriver) GetProvider() string {
	return api.CLOUD_PROVIDER_OPENSTACK
}

func (self *SOpenStackRegionDriver) IsVpcCreateNeedInputCidr() bool {
	return false
}

func (self *SOpenStackRegionDriver) ValidateCreateLoadbalancerData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, input *api.LoadbalancerCreateInput) (*api.LoadbalancerCreateInput, error) {
	// 公网ELB需要指定EIP
	if input.AddressType == api.LB_ADDR_TYPE_INTERNET && len(input.EipId) == 0 {
		return input, httperrors.NewMissingParameterError("eip_id")
	}
	return self.SManagedVirtualizationRegionDriver.ValidateCreateLoadbalancerData(ctx, userCred, ownerId, input)
}

func (self *SOpenStackRegionDriver) RequestCreateLoadbalancerAcl(ctx context.Context, userCred mcclient.TokenCredential, lbacl *models.SCachedLoadbalancerAcl, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return self.createLoadbalancerAcl(ctx, userCred, lbacl)
	})
	return nil
}

func (self *SOpenStackRegionDriver) ValidateCreateLoadbalancerCertificateData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	return nil, httperrors.NewNotImplementedError("%s does not currently support creating loadbalancer certificate", self.GetProvider())
}

func (self *SOpenStackRegionDriver) ValidateCreateEipData(ctx context.Context, userCred mcclient.TokenCredential, input *api.SElasticipCreateInput) error {
	if len(input.NetworkId) == 0 {
		return httperrors.NewMissingParameterError("network_id")
	}
	_network, err := models.NetworkManager.FetchByIdOrName(userCred, input.NetworkId)
	if err != nil {
		if err == sql.ErrNoRows {
			return httperrors.NewResourceNotFoundError2("network", input.NetworkId)
		}
		return httperrors.NewGeneralError(err)
	}
	network := _network.(*models.SNetwork)
	input.NetworkId = network.Id

	vpc, _ := network.GetVpc()
	if vpc == nil {
		return httperrors.NewInputParameterError("failed to found vpc for network %s(%s)", network.Name, network.Id)
	}
	input.ManagerId = vpc.ManagerId
	region, err := vpc.GetRegion()
	if err != nil {
		return err
	}
	if region.Id != input.CloudregionId {
		return httperrors.NewUnsupportOperationError("network %s(%s) does not belong to %s", network.Name, network.Id, self.GetProvider())
	}
	return nil
}

func (self *SOpenStackRegionDriver) ValidateCreateLoadbalancerListenerData(ctx context.Context, userCred mcclient.TokenCredential,
	ownerId mcclient.IIdentityProvider, input *api.LoadbalancerListenerCreateInput,
	lb *models.SLoadbalancer, lbbg *models.SLoadbalancerBackendGroup) (*api.LoadbalancerListenerCreateInput, error) {
	return input, httperrors.NewNotImplementedError("ValidateCreateLoadbalancerListenerData")
}

func (self *SOpenStackRegionDriver) RequestCreateLoadbalancerListener(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})
	return nil
}

func (self *SOpenStackRegionDriver) RequestSyncLoadbalancerListener(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})
	return nil
}

func (self *SOpenStackRegionDriver) IsSupportLoadbalancerListenerRuleRedirect() bool {
	return true
}

func (self *SOpenStackRegionDriver) ValidateUpdateLoadbalancerListenerRuleData(ctx context.Context, userCred mcclient.TokenCredential, input *api.LoadbalancerListenerRuleUpdateInput) (*api.LoadbalancerListenerRuleUpdateInput, error) {
	return input, nil
}

func (self *SOpenStackRegionDriver) RequestCreateLoadbalancerListenerRule(ctx context.Context, userCred mcclient.TokenCredential, lbr *models.SLoadbalancerListenerRule, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})
	return nil
}

func (self *SOpenStackRegionDriver) RequestDeleteLoadbalancerListenerRule(ctx context.Context, userCred mcclient.TokenCredential, lbr *models.SLoadbalancerListenerRule, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})
	return nil
}

func (self *SOpenStackRegionDriver) RequestSyncLoadbalancerBackendGroup(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})

	return nil
}

func (self *SOpenStackRegionDriver) ValidateDeleteLoadbalancerBackendGroupCondition(ctx context.Context, lbbg *models.SLoadbalancerBackendGroup) error {
	// pool不能被l7policy关联，若要解除关联关系，可通过更新转发策略将转测策略的redirect_pool_id更新为null。
	count, err := lbbg.RefCount()
	if err != nil {
		return err
	}

	if count != 0 {
		return fmt.Errorf("backendgroup is binding with loadbalancer/listener/listenerrule.")
	}

	return nil
}

func (self *SOpenStackRegionDriver) RequestDeleteLoadbalancerBackendGroup(ctx context.Context, userCred mcclient.TokenCredential, lbbg *models.SLoadbalancerBackendGroup, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})
	return nil
}

func (self *SOpenStackRegionDriver) RequestCreateLoadbalancerBackend(ctx context.Context, userCred mcclient.TokenCredential, lbb *models.SLoadbalancerBackend, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})
	return nil
}

func (self *SOpenStackRegionDriver) RequestSyncLoadbalancerBackend(ctx context.Context, userCred mcclient.TokenCredential, lbb *models.SLoadbalancerBackend, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})
	return nil
}

func (self *SOpenStackRegionDriver) RequestDeleteLoadbalancerBackend(ctx context.Context, userCred mcclient.TokenCredential, lbb *models.SLoadbalancerBackend, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		return nil, cloudprovider.ErrNotImplemented
	})
	return nil
}
