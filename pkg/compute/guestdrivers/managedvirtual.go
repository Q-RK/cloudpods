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

package guestdrivers

import (
	"context"
	"fmt"
	"math"
	"time"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/utils"
	"yunion.io/x/sqlchemy"

	billing_api "yunion.io/x/onecloud/pkg/apis/billing"
	api "yunion.io/x/onecloud/pkg/apis/compute"
	"yunion.io/x/onecloud/pkg/cloudcommon/db"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/lockman"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/taskman"
	"yunion.io/x/onecloud/pkg/compute/models"
	"yunion.io/x/onecloud/pkg/compute/options"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/mcclient"
	"yunion.io/x/onecloud/pkg/util/billing"
	"yunion.io/x/onecloud/pkg/util/cloudinit"
	"yunion.io/x/onecloud/pkg/util/logclient"
	"yunion.io/x/onecloud/pkg/util/pinyinutils"
)

type SManagedVirtualizedGuestDriver struct {
	SVirtualizedGuestDriver
}

func (d SManagedVirtualizedGuestDriver) DoScheduleCPUFilter() bool { return false }

func (d SManagedVirtualizedGuestDriver) DoScheduleSKUFilter() bool { return true }

func (d SManagedVirtualizedGuestDriver) DoScheduleMemoryFilter() bool { return false }

func (d SManagedVirtualizedGuestDriver) DoScheduleStorageFilter() bool { return false }

func (d SManagedVirtualizedGuestDriver) DoScheduleCloudproviderTagFilter() bool { return true }

func (self *SManagedVirtualizedGuestDriver) GetJsonDescAtHost(ctx context.Context, userCred mcclient.TokenCredential, guest *models.SGuest, host *models.SHost, params *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
	config := cloudprovider.SManagedVMCreateConfig{
		IsNeedInjectPasswordByCloudInit: guest.GetDriver().IsNeedInjectPasswordByCloudInit(),
		UserDataType:                    guest.GetDriver().GetUserDataType(),
		WindowsUserDataType:             guest.GetDriver().GetWindowsUserDataType(),
		IsWindowsUserDataTypeNeedEncode: guest.GetDriver().IsWindowsUserDataTypeNeedEncode(),
	}
	config.Name = guest.Name
	config.NameEn = pinyinutils.Text2Pinyin(guest.Name)
	config.Hostname = guest.Hostname
	config.Cpu = int(guest.VcpuCount)
	config.MemoryMB = guest.VmemSize
	config.UserData = guest.GetUserData(ctx, userCred)
	config.Description = guest.Description
	config.EnableMonitorAgent = options.Options.EnableMonitorAgent
	if params != nil {
		params.Unmarshal(&config.SPublicIpInfo)
	}

	config.InstanceType = guest.InstanceType

	if len(guest.KeypairId) > 0 {
		config.PublicKey = guest.GetKeypairPublicKey()
	}

	nics, _ := guest.GetNetworks("")
	if len(nics) > 0 {
		net := nics[0].GetNetwork()
		config.ExternalNetworkId = net.ExternalId
		vpc, err := net.GetVpc()
		if err == nil {
			config.ExternalVpcId = vpc.ExternalId
		}
		config.IpAddr = nics[0].IpAddr
	}

	var err error
	provider := host.GetCloudprovider()
	config.ProjectId, err = provider.SyncProject(ctx, userCred, guest.ProjectId)
	if err != nil {
		log.Errorf("failed to sync project %s for create %s guest %s error: %v", guest.ProjectId, provider.Provider, guest.Name, err)
	}

	disks, err := guest.GetDisks()
	if err != nil {
		return nil, errors.Wrapf(err, "GetDisks")
	}
	config.DataDisks = []cloudprovider.SDiskInfo{}

	for i := 0; i < len(disks); i += 1 {
		disk := disks[i]
		storage, _ := disk.GetStorage()
		if i == 0 {
			config.SysDisk.Name = disk.Name
			config.SysDisk.StorageExternalId = storage.ExternalId
			config.SysDisk.StorageType = storage.StorageType
			config.SysDisk.SizeGB = int(math.Ceil(float64(disk.DiskSize) / 1024))
			cache := storage.GetStoragecache()
			imageId := disk.GetTemplateId()
			//避免因同步过来的instance没有对应的imagecache信息，重置密码时引发空指针访问
			if scimg := models.StoragecachedimageManager.GetStoragecachedimage(cache.Id, imageId); scimg != nil {
				config.ExternalImageId = scimg.ExternalId
				img := scimg.GetCachedimage()
				config.OsDistribution, _ = img.Info.GetString("properties", "os_distribution")
				config.OsVersion, _ = img.Info.GetString("properties", "os_version")
				config.OsType, _ = img.Info.GetString("properties", "os_type")
				config.ImageType = img.ImageType
			}
		} else {
			dataDisk := cloudprovider.SDiskInfo{
				SizeGB:            disk.DiskSize / 1024,
				StorageType:       storage.StorageType,
				StorageExternalId: storage.ExternalId,
				Name:              disk.Name,
			}
			config.DataDisks = append(config.DataDisks, dataDisk)
		}
	}

	// 避免因同步包年包月实例billing_cycle失败,导致重置虚拟机密码异常
	if guest.BillingType == billing_api.BILLING_TYPE_PREPAID && len(guest.BillingCycle) > 0 {
		bc, err := billing.ParseBillingCycle(guest.BillingCycle)
		if err != nil {
			return nil, errors.Wrapf(err, "ParseBillingCycle(%s)", guest.BillingCycle)
		}
		if bc.IsValid() {
			bc.AutoRenew = guest.AutoRenew
			config.BillingCycle = &bc
		}
	}

	return jsonutils.Marshal(&config), nil
}

func (self *SManagedVirtualizedGuestDriver) RequestSaveImage(ctx context.Context, userCred mcclient.TokenCredential, guest *models.SGuest, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		iVm, err := guest.GetIVM(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "guest.GetIVM")
		}
		opts := &cloudprovider.SaveImageOptions{}
		err = task.GetParams().Unmarshal(opts)
		image, err := iVm.SaveImage(opts)
		if err != nil {
			return nil, errors.Wrapf(err, "iVm.SaveImage")
		}
		err = cloudprovider.WaitStatus(image, cloudprovider.IMAGE_STATUS_ACTIVE, time.Second*10, time.Minute*10)
		if err != nil {
			return nil, errors.Wrapf(err, "wait image %s(%s) active current is: %s", image.GetName(), image.GetGlobalId(), image.GetStatus())
		}
		host, _ := guest.GetHost()
		if host == nil {
			return nil, errors.Wrapf(cloudprovider.ErrNotFound, "find guest %s host", guest.Name)
		}
		region, _ := host.GetRegion()
		iRegion, err := host.GetIRegion(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "host.GetIRegion")
		}
		caches, err := region.GetStoragecaches()
		if err != nil {
			return nil, errors.Wrapf(err, "region.GetStoragecaches")
		}
		for i := range caches {
			if caches[i].ManagerId == host.ManagerId {
				iStoragecache, err := iRegion.GetIStoragecacheById(caches[i].ExternalId)
				if err != nil {
					return nil, errors.Wrapf(err, "iRegion.GetIStoragecacheById(%s)", caches[i].ExternalId)
				}
				result := caches[i].SyncCloudImages(ctx, userCred, iStoragecache, region)
				log.Infof("sync cloud image for storagecache %s result: %s", caches[i].Name, result.Result())
			}
		}
		return nil, nil
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestGuestCreateAllDisks(ctx context.Context, guest *models.SGuest, task taskman.ITask) error {
	diskCat := guest.CategorizeDisks()
	var imageId string
	if diskCat.Root != nil {
		imageId = diskCat.Root.GetTemplateId()
	}
	if len(imageId) == 0 {
		task.ScheduleRun(nil)
		return nil
	}
	storage, _ := diskCat.Root.GetStorage()
	if storage == nil {
		return fmt.Errorf("no valid storage")
	}
	storageCache := storage.GetStoragecache()
	if storageCache == nil {
		return fmt.Errorf("no valid storage cache")
	}
	input := api.CacheImageInput{
		ImageId:      imageId,
		Format:       diskCat.Root.DiskFormat,
		ParentTaskId: task.GetTaskId(),
		ServerId:     guest.Id,
	}
	return storageCache.StartImageCacheTask(ctx, task.GetUserCred(), input)
}

func (self *SManagedVirtualizedGuestDriver) ValidateCreateData(ctx context.Context, userCred mcclient.TokenCredential, input *api.ServerCreateInput) (*api.ServerCreateInput, error) {
	if input.Cdrom != "" {
		return nil, httperrors.NewInputParameterError("%s not support cdrom params", input.Hypervisor)
	}
	driver := models.GetDriver(input.Hypervisor)
	if len(input.UserData) > 0 && driver != nil && driver.IsNeedInjectPasswordByCloudInit() {
		_, err := cloudinit.ParseUserData(input.UserData)
		if err != nil {
			return nil, err
		}
	}
	return input, nil
}

func (self *SManagedVirtualizedGuestDriver) ValidateCreateEip(ctx context.Context, userCred mcclient.TokenCredential, input api.ServerCreateEipInput) error {
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestDetachDisk(ctx context.Context, guest *models.SGuest, disk *models.SDisk, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		iVM, err := guest.GetIVM(ctx)
		if err != nil {
			//若guest被删除,忽略错误，否则会无限删除guest失败(有挂载的云盘)
			if errors.Cause(err) == cloudprovider.ErrNotFound {
				return nil, nil
			}
			return nil, errors.Wrapf(err, "guest.GetIVM")
		}
		if len(disk.ExternalId) == 0 {
			return nil, nil
		}

		_, err = disk.GetIDisk(ctx)
		if errors.Cause(err) == cloudprovider.ErrNotFound {
			//忽略云上磁盘已经被删除错误
			return nil, nil
		}

		err = iVM.DetachDisk(ctx, disk.ExternalId)
		if err != nil {
			return nil, errors.Wrapf(err, "iVM.DetachDisk")
		}

		err = cloudprovider.Wait(time.Second*5, time.Minute*3, func() (bool, error) {
			err := iVM.Refresh()
			if err != nil {
				return false, errors.Wrapf(err, "iVM.Refresh")
			}
			iDisks, err := iVM.GetIDisks()
			if err != nil {
				return false, errors.Wrapf(err, "RequestDetachDisk.iVM.GetIDisks")
			}

			exist := false
			for i := 0; i < len(iDisks); i++ {
				if iDisks[i].GetGlobalId() == disk.ExternalId {
					exist = true
				}
			}

			if !exist {
				return true, nil
			}
			return false, nil
		})

		if err != nil {
			return nil, errors.Wrapf(err, "RequestDetachDisk.Wait")
		}

		return nil, nil
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestAttachDisk(ctx context.Context, guest *models.SGuest, disk *models.SDisk, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		iVM, err := guest.GetIVM(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "guest.GetIVM")
		}
		if len(disk.ExternalId) == 0 {
			return nil, fmt.Errorf("disk %s(%s) is not a managed resource", disk.Name, disk.Id)
		}
		err = iVM.AttachDisk(ctx, disk.ExternalId)
		if err != nil {
			return nil, errors.Wrapf(err, "iVM.AttachDisk")
		}

		err = cloudprovider.Wait(time.Second*10, time.Minute*6, func() (bool, error) {
			err := iVM.Refresh()
			if err != nil {
				return false, errors.Wrapf(err, "iVM.Refresh")
			}

			iDisks, err := iVM.GetIDisks()
			if err != nil {
				return false, errors.Wrapf(err, "RequestAttachDisk.iVM.GetIDisks")
			}

			for i := 0; i < len(iDisks); i++ {
				if iDisks[i].GetGlobalId() == disk.ExternalId {
					err := cloudprovider.WaitStatus(iDisks[i], api.DISK_READY, 5*time.Second, 60*time.Second)
					if err != nil {
						return false, errors.Wrapf(err, "RequestAttachDisk.iVM.WaitStatus")
					}

					return true, nil
				}
			}

			return false, nil
		})

		if err != nil {
			return nil, errors.Wrapf(err, "RequestAttachDisk.Wait")
		}

		return nil, nil
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestStartOnHost(ctx context.Context, guest *models.SGuest, host *models.SHost, userCred mcclient.TokenCredential, task taskman.ITask) error {
	ivm, err := guest.GetIVM(ctx)
	if err != nil {
		return errors.Wrapf(err, "GetIVM")
	}

	result := jsonutils.NewDict()
	if ivm.GetStatus() != api.VM_RUNNING {
		err := ivm.StartVM(ctx)
		if err != nil {
			return errors.Wrapf(err, "StartVM")
		}
		err = cloudprovider.WaitStatus(ivm, api.VM_RUNNING, time.Second*5, time.Minute*10)
		if err != nil {
			return errors.Wrapf(err, "Wait vm running")
		}
		// 虚拟机开机，公网ip自动生成
		guest.SyncAllWithCloudVM(ctx, userCred, host, ivm, true)
		return task.ScheduleRun(result)
	}
	return guest.SetStatus(userCred, api.VM_RUNNING, "StartOnHost")
}

func (self *SManagedVirtualizedGuestDriver) RequestDeployGuestOnHost(ctx context.Context, guest *models.SGuest, host *models.SHost, task taskman.ITask) error {
	config, err := guest.GetDeployConfigOnHost(ctx, task.GetUserCred(), host, task.GetParams())
	if err != nil {
		return errors.Wrapf(err, "GetDeployConfigOnHost")
	}
	log.Debugf("RequestDeployGuestOnHost: %s", config)

	desc := cloudprovider.SManagedVMCreateConfig{}
	// 账号必须在desc.GetConfig()之前设置，避免默认用户不能正常注入
	desc.Account = guest.GetDriver().GetDefaultAccount(desc)
	err = desc.GetConfig(config)
	if err != nil {
		return errors.Wrapf(err, "desc.GetConfig")
	}

	desc.Tags, _ = guest.GetAllUserMetadata()

	//创建并同步安全组规则, 仅新建的安全组会同步规则
	{
		vpc, err := guest.GetVpc()
		if err != nil {
			return errors.Wrap(err, "guest.GetVpc")
		}
		region, err := vpc.GetRegion()
		if err != nil {
			return errors.Wrap(err, "vpc.GetRegion")
		}

		vpcId, err := region.GetDriver().GetSecurityGroupVpcId(ctx, task.GetUserCred(), region, host, vpc, false)
		if err != nil {
			return errors.Wrap(err, "GetSecurityGroupVpcId")
		}

		secgroups, err := guest.GetSecgroups()
		if err != nil {
			return errors.Wrap(err, "GetSecgroups")
		}
		for i, secgroup := range secgroups {
			externalId, err := region.GetDriver().RequestSyncSecurityGroup(ctx, task.GetUserCred(), vpcId, vpc, &secgroup, desc.ProjectId, "", true)
			if err != nil {
				return errors.Wrap(err, "RequestSyncSecurityGroup")
			}

			desc.ExternalSecgroupIds = append(desc.ExternalSecgroupIds, externalId)
			if i == 0 {
				desc.ExternalSecgroupId = externalId
			}
		}
	}

	desc.UserData, err = desc.GetUserData()
	if err != nil {
		return errors.Wrapf(err, "GetUserData")
	}

	action, err := config.GetString("action")
	if err != nil {
		return err
	}

	ihost, err := host.GetIHost(ctx)
	if err != nil {
		return err
	}

	switch action {
	case "create":
		region, _ := host.GetRegion()
		if len(desc.InstanceType) == 0 && region != nil && utils.IsInStringArray(guest.Hypervisor, api.PUBLIC_CLOUD_HYPERVISORS) {
			sku, err := models.ServerSkuManager.GetMatchedSku(region.GetId(), int64(desc.Cpu), int64(desc.MemoryMB))
			if err != nil {
				return errors.Wrap(err, "ManagedVirtualizedGuestDriver.RequestDeployGuestOnHost.GetMatchedSku")
			}

			if sku == nil {
				return errors.Wrap(errors.ErrNotFound, "ManagedVirtualizedGuestDriver.RequestDeployGuestOnHost.GetMatchedSku")
			}

			desc.InstanceType = sku.Name
		}

		taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
			return guest.GetDriver().RemoteDeployGuestForCreate(ctx, task.GetUserCred(), guest, host, desc)
		})
	case "deploy":
		taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
			return guest.GetDriver().RemoteDeployGuestForDeploy(ctx, guest, ihost, task, desc)
		})
	case "rebuild":
		taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
			return guest.GetDriver().RemoteDeployGuestForRebuildRoot(ctx, guest, ihost, task, desc)
		})
	default:
		log.Errorf("RequestDeployGuestOnHost: Action %s not supported", action)
		return fmt.Errorf("Action %s not supported", action)
	}
	return nil
}

func (self *SManagedVirtualizedGuestDriver) GetGuestInitialStateAfterCreate() string {
	return api.VM_READY
}

func (self *SManagedVirtualizedGuestDriver) GetGuestInitialStateAfterRebuild() string {
	return api.VM_READY
}

func (self *SManagedVirtualizedGuestDriver) RemoteDeployGuestForCreate(ctx context.Context, userCred mcclient.TokenCredential, guest *models.SGuest, host *models.SHost, desc cloudprovider.SManagedVMCreateConfig) (jsonutils.JSONObject, error) {
	ihost, err := host.GetIHost(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "RemoteDeployGuestForCreate.GetIHost")
	}

	iVM, err := func() (cloudprovider.ICloudVM, error) {
		lockman.LockObject(ctx, guest)
		defer lockman.ReleaseObject(ctx, guest)

		iVM, err := func() (cloudprovider.ICloudVM, error) {
			iVM, err := ihost.CreateVM(&desc)
			if err == nil || !options.Options.EnableAutoSwitchServerSku {
				return iVM, err
			}
			skus, e := models.ServerSkuManager.GetSkus(host.GetProviderName(), guest.VcpuCount, guest.VmemSize)
			if e != nil {
				return iVM, errors.Wrapf(e, "GetSkus")
			}
			oldSku := desc.InstanceType
			for i := range skus {
				if skus[i].Name != oldSku {
					desc.InstanceType = skus[i].Name
					log.Infof("try switch server sku from %s to %s for create %s", oldSku, desc.InstanceType, guest.Name)
					iVM, err = ihost.CreateVM(&desc)
					if err == nil {
						db.Update(guest, func() error {
							guest.InstanceType = desc.InstanceType
							return nil
						})
						return iVM, nil
					}
				}
			}
			return iVM, err
		}()
		if err != nil {
			return nil, err
		}

		db.SetExternalId(guest, userCred, iVM.GetGlobalId())
		return iVM, nil
	}()
	if err != nil {
		return nil, err
	}
	// iVM 实际所在的ihost 可能和 调度选择的host不是同一个,此处根据iVM实际所在host，重新同步
	ihost, err = guest.GetDriver().RemoteDeployGuestSyncHost(ctx, userCred, guest, host, iVM)
	if err != nil {
		return nil, errors.Wrap(err, "RemoteDeployGuestSyncHost")
	}

	initialState := guest.GetDriver().GetGuestInitialStateAfterCreate()
	log.Debugf("VMcreated %s, wait status %s ...", iVM.GetGlobalId(), initialState)
	err = cloudprovider.WaitStatusWithInstanceErrorCheck(iVM, initialState, time.Second*5, time.Second*1800, func() error {
		return iVM.GetError()
	})
	if err != nil {
		return nil, err
	}
	log.Debugf("VMcreated %s, and status is running", iVM.GetGlobalId())

	iVM, err = ihost.GetIVMById(iVM.GetGlobalId())
	if err != nil {
		return nil, errors.Wrapf(err, "GetIVMById(%s)", iVM.GetGlobalId())
	}

	if guest.GetDriver().GetMaxSecurityGroupCount() > 0 {
		err = iVM.SetSecurityGroups(desc.ExternalSecgroupIds)
		if err != nil {
			return nil, errors.Wrapf(err, "SetSecurityGroups")
		}
	}

	ret, expect, eipSync := 0, len(desc.DataDisks)+1, false
	err = cloudprovider.RetryUntil(func() (bool, error) {
		if desc.PublicIpBw > 0 {
			eip, _ := iVM.GetIEIP()
			if eip == nil {
				iVM.Refresh()
				return false, nil
			}
			// 同步静态公网ip
			if !eipSync {
				provider := host.GetCloudprovider()
				guest.SyncVMEip(ctx, userCred, provider, eip, provider.GetOwnerId())
				eipSync = true
			}
		}

		idisks, err := iVM.GetIDisks()
		if err != nil {
			return false, errors.Wrap(err, "iVM.GetIDisks")
		}
		ret = len(idisks)
		if ret >= expect { // 有可能自定义镜像里面也有磁盘，会导致返回的磁盘多于创建时的磁盘
			return true, nil
		}
		return false, nil
	}, 10)
	if err != nil {
		return nil, errors.Wrapf(err, "GuestDriver.RemoteDeployGuestForCreate.RetryUntil expect %d disks return %d disks", expect, ret)
	}

	// 回填IP
	if len(desc.IpAddr) == 0 {
		nics, _ := iVM.GetINics()
		gns, _ := guest.GetNetworks("")
		if len(nics) > 0 && len(gns) > 0 {
			db.Update(&gns[0], func() error {
				gns[0].IpAddr = nics[0].GetIP()
				gns[0].MacAddr = nics[0].GetMAC()
				gns[0].Driver = nics[0].GetDriver()
				return nil
			})
		}
	}

	guest.GetDriver().RemoteActionAfterGuestCreated(ctx, userCred, guest, host, iVM, &desc)

	data := fetchIVMinfo(desc, iVM, guest.Id, desc.Account, desc.Password, desc.PublicKey, "create")
	return data, nil
}

func (self *SManagedVirtualizedGuestDriver) RemoteDeployGuestSyncHost(ctx context.Context, userCred mcclient.TokenCredential, guest *models.SGuest, host *models.SHost, iVM cloudprovider.ICloudVM) (cloudprovider.ICloudHost, error) {
	if hostId := iVM.GetIHostId(); len(hostId) > 0 {
		nh, err := db.FetchByExternalIdAndManagerId(models.HostManager, hostId, func(q *sqlchemy.SQuery) *sqlchemy.SQuery {
			return q.Equals("manager_id", host.ManagerId)
		})
		if err != nil {
			log.Warningf("failed to found new hostId(%s) for ivm %s(%s) error: %v", hostId, guest.Name, guest.Id, err)
		} else if nh.GetId() != guest.HostId {
			guest.OnScheduleToHost(ctx, userCred, nh.GetId())
			host = nh.(*models.SHost)
		}
	}

	return host.GetIHost(ctx)
}

func (self *SManagedVirtualizedGuestDriver) RemoteDeployGuestForDeploy(ctx context.Context, guest *models.SGuest, ihost cloudprovider.ICloudHost, task taskman.ITask, desc cloudprovider.SManagedVMCreateConfig) (jsonutils.JSONObject, error) {
	iVM, err := ihost.GetIVMById(guest.GetExternalId())
	if err != nil || iVM == nil {
		log.Errorf("cannot find vm %s", err)
		return nil, fmt.Errorf("cannot find vm")
	}

	params := task.GetParams()
	log.Debugf("Deploy VM params %s", params.String())

	deleteKeypair := jsonutils.QueryBoolean(params, "__delete_keypair__", false)

	if len(desc.UserData) > 0 {
		err := iVM.UpdateUserData(desc.UserData)
		if err != nil {
			log.Errorf("update userdata fail %s", err)
		}
	}

	err = func() error {
		lockman.LockObject(ctx, guest)
		defer lockman.ReleaseObject(ctx, guest)

		// 避免DeployVM函数里面执行顺序不一致导致与预期结果不符
		if deleteKeypair {
			desc.Password, desc.PublicKey = "", ""
		}

		if len(desc.PublicKey) > 0 {
			desc.Password = ""
		}

		e := iVM.DeployVM(ctx, desc.Name, desc.Account, desc.Password, desc.PublicKey, deleteKeypair, desc.Description)
		if e != nil {
			return e
		}

		if len(desc.Password) == 0 {
			//可以从秘钥解密旧密码
			desc.Password = guest.GetOldPassword(ctx, task.GetUserCred())
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}

	data := fetchIVMinfo(desc, iVM, guest.Id, desc.Account, desc.Password, desc.PublicKey, "deploy")

	return data, nil
}

func (self *SManagedVirtualizedGuestDriver) RemoteDeployGuestForRebuildRoot(ctx context.Context, guest *models.SGuest, ihost cloudprovider.ICloudHost, task taskman.ITask, desc cloudprovider.SManagedVMCreateConfig) (jsonutils.JSONObject, error) {
	iVM, err := ihost.GetIVMById(guest.GetExternalId())
	if err != nil {
		return nil, errors.Wrapf(err, "ihost.GetIVMById(%s)", guest.GetExternalId())
	}

	if len(desc.UserData) > 0 {
		err := iVM.UpdateUserData(desc.UserData)
		if err != nil {
			log.Errorf("update userdata fail %s", err)
		}
		cloudprovider.WaitMultiStatus(iVM, []string{api.VM_READY, api.VM_RUNNING}, time.Second*5, time.Minute*3)
	}

	diskId, err := func() (string, error) {
		lockman.LockObject(ctx, guest)
		defer lockman.ReleaseObject(ctx, guest)

		conf := cloudprovider.SManagedVMRebuildRootConfig{
			Account:   desc.Account,
			ImageId:   desc.ExternalImageId,
			Password:  desc.Password,
			PublicKey: desc.PublicKey,
			SysSizeGB: desc.SysDisk.SizeGB,
			OsType:    desc.OsType,
		}
		return iVM.RebuildRoot(ctx, &conf)
	}()
	if err != nil {
		return nil, err
	}

	initialState := guest.GetDriver().GetGuestInitialStateAfterRebuild()
	log.Debugf("VMrebuildRoot %s new diskID %s, wait status %s ...", iVM.GetGlobalId(), diskId, initialState)
	err = cloudprovider.WaitStatus(iVM, initialState, time.Second*5, time.Second*1800)
	if err != nil {
		return nil, err
	}
	log.Debugf("VMrebuildRoot %s, and status is ready", iVM.GetGlobalId())

	maxWaitSecs := 300
	waited := 0

	for {
		// hack, wait disk number consistent
		idisks, err := iVM.GetIDisks()
		if err != nil {
			log.Errorf("fail to find VM idisks %s", err)
			return nil, err
		}
		if len(idisks) < len(desc.DataDisks)+1 {
			if waited > maxWaitSecs {
				return nil, errors.Wrapf(cloudprovider.ErrTimeout, "inconsistent disk number %d < %d, wait timeout, must be something wrong on remote", len(idisks), len(desc.DataDisks)+1)
			}
			log.Debugf("inconsistent disk number???? %d != %d", len(idisks), len(desc.DataDisks)+1)
			time.Sleep(time.Second * 5)
			waited += 5
		} else {
			if idisks[0].GetGlobalId() == diskId {
				break
			}
			if waited > maxWaitSecs {
				return nil, fmt.Errorf("inconsistent sys disk id after rebuild root")
			}
			log.Debugf("current system disk id inconsistent %s != %s, try after 5 seconds", idisks[0].GetGlobalId(), diskId)
			time.Sleep(time.Second * 5)
			waited += 5
		}
	}

	data := fetchIVMinfo(desc, iVM, guest.Id, desc.Account, desc.Password, desc.PublicKey, "rebuild")

	return data, nil
}

func (self *SManagedVirtualizedGuestDriver) RequestUndeployGuestOnHost(ctx context.Context, guest *models.SGuest, host *models.SHost, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		ihost, err := host.GetIHost(ctx)
		if err != nil {
			//私有云宿主机有可能下线,会导致虚拟机无限删除失败
			if errors.Cause(err) == cloudprovider.ErrNotFound {
				return nil, nil
			}
			return nil, errors.Wrapf(err, "host.GetIHost")
		}

		// 创建失败时external id为空。此时直接返回即可。不需要再调用公有云api
		if len(guest.ExternalId) == 0 {
			return nil, nil
		}

		ivm, err := ihost.GetIVMById(guest.ExternalId)
		if err != nil {
			if errors.Cause(err) == cloudprovider.ErrNotFound {
				return nil, nil
			}
			return nil, errors.Wrapf(err, "ihost.GetIVMById(%s)", guest.ExternalId)
		}
		err = ivm.DeleteVM(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "ivm.DeleteVM")
		}

		disks, err := guest.GetDisks()
		if err != nil {
			return nil, errors.Wrapf(err, "GetDisks")
		}

		for _, disk := range disks {
			storage, _ := disk.GetStorage()
			if disk.AutoDelete && !utils.IsInStringArray(storage.StorageType, api.STORAGE_LOCAL_TYPES) {
				idisk, err := disk.GetIDisk(ctx)
				if err != nil {
					if errors.Cause(err) == cloudprovider.ErrNotFound {
						continue
					}
					return nil, errors.Wrapf(err, "disk.GetIDisk")
				}
				if idisk.GetStatus() == api.DISK_DEALLOC {
					continue
				}
				err = idisk.Delete(ctx)
				if err != nil {
					return nil, errors.Wrapf(err, "idisk.Delete")
				}
			}
		}
		return nil, nil
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestStopOnHost(ctx context.Context, guest *models.SGuest, host *models.SHost, task taskman.ITask, syncStatus bool) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		ivm, err := guest.GetIVM(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "guest.GetIVM")
		}
		if ivm.GetStatus() != api.VM_READY {
			opts := &cloudprovider.ServerStopOptions{}
			task.GetParams().Unmarshal(opts)
			err = ivm.StopVM(ctx, opts)
			if err != nil {
				return nil, errors.Wrapf(err, "ivm.StopVM")
			}
			err = cloudprovider.WaitStatus(ivm, api.VM_READY, time.Second*3, time.Minute*5)
			if err != nil {
				return nil, errors.Wrapf(err, "wait server stop after 5 miniutes")
			}
		}
		// 公有云关机，公网ip会释放
		guest.SyncAllWithCloudVM(ctx, task.GetUserCred(), host, ivm, syncStatus)
		return nil, nil
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestSyncstatusOnHost(ctx context.Context, guest *models.SGuest, host *models.SHost, userCred mcclient.TokenCredential, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		ihost, err := host.GetIHost(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "host.GetIHost")
		}
		ivm, err := ihost.GetIVMById(guest.ExternalId)
		if err != nil {
			log.Errorf("fail to find ivm by id %s", err)
			return nil, errors.Wrap(err, "ihost.GetIVMById")
		}

		err = guest.SyncAllWithCloudVM(ctx, userCred, host, ivm, true)
		if err != nil {
			return nil, errors.Wrap(err, "guest.SyncAllWithCloudVM")
		}

		status := GetCloudVMStatus(ivm)
		body := jsonutils.NewDict()
		body.Add(jsonutils.NewString(status), "status")
		return body, nil
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) GetGuestVncInfo(ctx context.Context, userCred mcclient.TokenCredential, guest *models.SGuest, host *models.SHost, input *cloudprovider.ServerVncInput) (*cloudprovider.ServerVncOutput, error) {
	ihost, err := host.GetIHost(ctx)
	if err != nil {
		return nil, err
	}

	iVM, err := ihost.GetIVMById(guest.ExternalId)
	if err != nil {
		log.Errorf("cannot find vm %s %s", iVM, err)
		return nil, err
	}

	return iVM.GetVNCInfo(input)
}

func (self *SManagedVirtualizedGuestDriver) RequestRebuildRootDisk(ctx context.Context, guest *models.SGuest, task taskman.ITask) error {
	subtask, err := taskman.TaskManager.NewTask(ctx, "ManagedGuestRebuildRootTask", guest, task.GetUserCred(), task.GetParams(), task.GetTaskId(), "", nil)
	if err != nil {
		return err
	}
	subtask.ScheduleRun(nil)
	return nil
}

func (self *SManagedVirtualizedGuestDriver) DoGuestCreateDisksTask(ctx context.Context, guest *models.SGuest, task taskman.ITask) error {
	subtask, err := taskman.TaskManager.NewTask(ctx, "ManagedGuestCreateDiskTask", guest, task.GetUserCred(), task.GetParams(), task.GetTaskId(), "", nil)
	if err != nil {
		return err
	}
	subtask.ScheduleRun(nil)
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestChangeVmConfig(ctx context.Context, guest *models.SGuest, task taskman.ITask, instanceType string, vcpuCount, vmemSize int64) error {
	host, _ := guest.GetHost()
	ihost, err := host.GetIHost(ctx)
	if err != nil {
		return err
	}

	iVM, err := ihost.GetIVMById(guest.GetExternalId())
	if err != nil {
		return err
	}

	if len(instanceType) == 0 {
		region, _ := host.GetRegion()
		sku, err := models.ServerSkuManager.GetMatchedSku(region.GetId(), vcpuCount, vmemSize)
		if err != nil {
			return errors.Wrap(err, "ManagedVirtualizedGuestDriver.RequestChangeVmConfig.GetMatchedSku")
		}

		if sku == nil {
			return errors.Wrap(errors.ErrNotFound, "ManagedVirtualizedGuestDriver.RequestChangeVmConfig.GetMatchedSku")
		}

		instanceType = sku.Name
	}

	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		config := &cloudprovider.SManagedVMChangeConfig{
			Cpu:          int(vcpuCount),
			MemoryMB:     int(vmemSize),
			InstanceType: instanceType,
		}
		err := iVM.ChangeConfig(ctx, config)
		if err != nil {
			return nil, errors.Wrap(err, "GuestDriver.RequestChangeVmConfig.ChangeConfig")
		}

		err = cloudprovider.WaitCreated(time.Second*5, time.Minute*5, func() bool {
			err := iVM.Refresh()
			if err != nil {
				return false
			}
			status := iVM.GetStatus()
			if status == api.VM_READY || status == api.VM_RUNNING {
				iInstanceType := iVM.GetInstanceType()
				if len(instanceType) > 0 && len(iInstanceType) > 0 && instanceType == iInstanceType {
					return true
				} else {
					// aws 目前取不到内存。返回值永远为0
					if iVM.GetVcpuCount() == int(vcpuCount) && (iVM.GetVmemSizeMB() == int(vmemSize) || iVM.GetVmemSizeMB() == 0) {
						return true
					}
				}
			}
			return false
		})
		if err != nil {
			return nil, errors.Wrap(err, "GuestDriver.RequestChangeVmConfig.WaitCreated")
		}

		instanceType = iVM.GetInstanceType()
		if len(instanceType) > 0 {
			_, err = db.Update(guest, func() error {
				guest.InstanceType = instanceType
				return nil
			})
			if err != nil {
				return nil, errors.Wrap(err, "GuestDriver.RequestChangeVmConfig.Update")
			}
		}

		return nil, nil
	})

	return nil
}

func (self *SManagedVirtualizedGuestDriver) OnGuestDeployTaskDataReceived(ctx context.Context, guest *models.SGuest, task taskman.ITask, data jsonutils.JSONObject) error {

	uuid, _ := data.GetString("uuid")
	if len(uuid) > 0 {
		db.SetExternalId(guest, task.GetUserCred(), uuid)
	}

	recycle := false
	if guest.IsPrepaidRecycle() {
		recycle = true
	}

	if data.Contains("disks") {
		diskInfo := make([]SDiskInfo, 0)
		err := data.Unmarshal(&diskInfo, "disks")
		if err != nil {
			return err
		}

		disks, _ := guest.GetGuestDisks()
		if len(disks) != len(diskInfo) {
			// 公有云镜像可能包含数据盘, 若忽略设置磁盘的external id, 会导致部分磁盘状态异常
			log.Warningf("inconsistent disk number: guest have %d disks, data contains %d disks", len(disks), len(diskInfo))
		}
		for i := 0; i < len(diskInfo) && i < len(disks); i += 1 {
			disk := disks[i].GetDisk()
			_, err = db.Update(disk, func() error {
				disk.DiskSize = diskInfo[i].Size
				disk.ExternalId = diskInfo[i].Uuid
				disk.DiskType = diskInfo[i].DiskType
				disk.Status = api.DISK_READY

				disk.FsFormat = diskInfo[i].FsFromat
				if diskInfo[i].AutoDelete {
					disk.AutoDelete = true
				}
				// disk.TemplateId = diskInfo[i].TemplateId
				disk.AccessPath = diskInfo[i].Path

				if !recycle {
					if len(diskInfo[i].BillingType) > 0 {
						disk.BillingType = diskInfo[i].BillingType
						disk.ExpiredAt = diskInfo[i].ExpiredAt
					}
				}

				if len(diskInfo[i].StorageExternalId) > 0 {
					storage, err := db.FetchByExternalIdAndManagerId(models.StorageManager, diskInfo[i].StorageExternalId, func(q *sqlchemy.SQuery) *sqlchemy.SQuery {
						host, _ := guest.GetHost()
						if host != nil {
							return q.Equals("manager_id", host.ManagerId)
						}
						return q
					})
					if err != nil {
						log.Warningf("failed to found storage by externalId %s error: %v", diskInfo[i].StorageExternalId, err)
					} else if disk.StorageId != storage.GetId() {
						disk.StorageId = storage.GetId()
					}
				}

				if len(diskInfo[i].Metadata) > 0 {
					for key, value := range diskInfo[i].Metadata {
						if err := disk.SetMetadata(ctx, key, value, task.GetUserCred()); err != nil {
							log.Errorf("set disk %s mata %s => %s error: %v", disk.Name, key, value, err)
						}
					}
				}
				return nil
			})
			if err != nil {
				msg := fmt.Sprintf("save disk info failed %s", err)
				log.Errorf(msg)
				break
			}
			db.OpsLog.LogEvent(disk, db.ACT_ALLOCATE, disk.GetShortDesc(ctx), task.GetUserCred())
			guestdisk := guest.GetGuestDisk(disk.Id)
			_, err = db.Update(guestdisk, func() error {
				guestdisk.Driver = diskInfo[i].Driver
				guestdisk.CacheMode = diskInfo[i].CacheMode
				return nil
			})
			if err != nil {
				msg := fmt.Sprintf("save disk info failed %s", err)
				log.Errorf(msg)
				break
			}
		}
	}

	if metaData, _ := data.Get("metadata"); metaData != nil {
		meta := make(map[string]string, 0)
		if err := metaData.Unmarshal(meta); err != nil {
			log.Errorf("Get guest %s metadata error: %v", guest.Name, err)
		} else {
			for key, value := range meta {
				if err := guest.SetMetadata(ctx, key, value, task.GetUserCred()); err != nil {
					log.Errorf("set guest %s mata %s => %s error: %v", guest.Name, key, value, err)
				}
			}
		}
	}

	exp, err := data.GetTime("expired_at")
	if err == nil && !guest.IsPrepaidRecycle() {
		guest.SaveRenewInfo(ctx, task.GetUserCred(), nil, &exp, "")
	}
	if guest.GetDriver().IsSupportSetAutoRenew() {
		autoRenew, _ := data.Bool("auto_renew")
		guest.SetAutoRenew(autoRenew)
	}

	guest.SaveDeployInfo(ctx, task.GetUserCred(), data)

	iVM, err := guest.GetIVM(ctx)
	if err != nil {
		return errors.Wrap(err, "guest.GetIVM")
	}
	guest.SyncOsInfo(ctx, task.GetUserCred(), iVM)
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestSyncSecgroupsOnHost(ctx context.Context, guest *models.SGuest, host *models.SHost, task taskman.ITask) error {
	iVM, err := guest.GetIVM(ctx)
	if err != nil {
		return err
	}

	vpc, err := guest.GetVpc()
	if err != nil {
		return errors.Wrap(err, "guest.GetVpc")
	}

	region, _ := host.GetRegion()

	vpcId, err := region.GetDriver().GetSecurityGroupVpcId(ctx, task.GetUserCred(), region, host, vpc, false)
	if err != nil {
		return errors.Wrap(err, "GetSecurityGroupVpcId")
	}

	remoteProjectId := ""
	provider := host.GetCloudprovider()
	if provider != nil {
		remoteProjectId, err = provider.SyncProject(ctx, task.GetUserCred(), guest.ProjectId)
		if err != nil {
			log.Errorf("failed to sync project %s for guest %s error: %v", guest.ProjectId, guest.Name, err)
		}
	}

	secgroups, err := guest.GetSecgroups()
	if err != nil {
		return errors.Wrap(err, "GetSecgroups")
	}
	externalIds := []string{}
	for _, secgroup := range secgroups {
		externalId, err := region.GetDriver().RequestSyncSecurityGroup(ctx, task.GetUserCred(), vpcId, vpc, &secgroup, remoteProjectId, "", true)
		if err != nil {
			return errors.Wrap(err, "RequestSyncSecurityGroup")
		}
		externalIds = append(externalIds, externalId)
	}
	return iVM.SetSecurityGroups(externalIds)
}

func (self *SManagedVirtualizedGuestDriver) RequestSyncConfigOnHost(ctx context.Context, guest *models.SGuest, host *models.SHost, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {

		if jsonutils.QueryBoolean(task.GetParams(), "fw_only", false) {
			err := guest.GetDriver().RequestSyncSecgroupsOnHost(ctx, guest, host, task)
			if err != nil {
				return nil, err
			}
		}

		return nil, nil
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestRenewInstance(ctx context.Context, guest *models.SGuest, bc billing.SBillingCycle) (time.Time, error) {
	iVM, err := guest.GetIVM(ctx)
	if err != nil {
		return time.Time{}, err
	}
	oldExpired := iVM.GetExpiredAt()
	err = iVM.Renew(bc)
	if err != nil {
		return time.Time{}, err
	}
	//避免有些云续费后过期时间刷新比较慢问题
	cloudprovider.WaitCreated(15*time.Second, 5*time.Minute, func() bool {
		err := iVM.Refresh()
		if err != nil {
			log.Errorf("failed refresh instance %s error: %v", guest.Name, err)
		}
		newExipred := iVM.GetExpiredAt()
		if newExipred.After(oldExpired) {
			return true
		}
		return false
	})
	return iVM.GetExpiredAt(), nil
}

func (self *SManagedVirtualizedGuestDriver) IsSupportEip() bool {
	return true
}

func (self *SManagedVirtualizedGuestDriver) chooseHostStorage(
	drv models.IGuestDriver,
	host *models.SHost,
	backend string,
	storageIds []string,
) *models.SStorage {
	if len(storageIds) != 0 {
		return models.StorageManager.FetchStorageById(storageIds[0])
	}
	storages := host.GetAttachedEnabledHostStorages(nil)
	for i := 0; i < len(storages); i += 1 {
		if storages[i].StorageType == backend {
			return &storages[i]
		}
	}
	for _, stype := range drv.GetStorageTypes() {
		for i := 0; i < len(storages); i += 1 {
			if storages[i].StorageType == stype {
				return &storages[i]
			}
		}
	}
	return nil
}

func (self *SManagedVirtualizedGuestDriver) IsSupportCdrom(guest *models.SGuest) (bool, error) {
	return false, nil
}

func (self *SManagedVirtualizedGuestDriver) IsSupportFloppy(guest *models.SGuest) (bool, error) {
	return false, nil
}

func GetCloudVMStatus(vm cloudprovider.ICloudVM) string {
	status := vm.GetStatus()
	switch status {
	case api.VM_RUNNING:
		status = cloudprovider.CloudVMStatusRunning
	case api.VM_READY:
		status = cloudprovider.CloudVMStatusStopped
	case api.VM_STARTING:
		status = cloudprovider.CloudVMStatusStopped
	case api.VM_STOPPING:
		status = cloudprovider.CloudVMStatusStopping
	case api.VM_CHANGE_FLAVOR:
		status = cloudprovider.CloudVMStatusChangeFlavor
	case api.VM_DEPLOYING:
		status = cloudprovider.CloudVMStatusDeploying
	case api.VM_SUSPEND:
		status = cloudprovider.CloudVMStatusSuspend
	default:
		status = cloudprovider.CloudVMStatusOther
	}

	return status
}

func (self *SManagedVirtualizedGuestDriver) RequestConvertPublicipToEip(ctx context.Context, userCred mcclient.TokenCredential, guest *models.SGuest, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		iVM, err := guest.GetIVM(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "guest.GetIVM")
		}
		err = iVM.ConvertPublicIpToEip()
		if err != nil {
			return nil, errors.Wrap(err, "iVM.ConvertPublicIpToEip")
		}

		publicIp, err := guest.GetPublicIp()
		if err != nil {
			return nil, errors.Wrap(err, "guest.GetPublicIp")
		}
		if publicIp == nil {
			return nil, fmt.Errorf("faild to found public ip after convert")
		}

		err = cloudprovider.Wait(time.Second*5, time.Minute*5, func() (bool, error) {
			err = iVM.Refresh()
			if err != nil {
				log.Errorf("refresh ivm error: %v", err)
				return false, nil
			}
			eip, err := iVM.GetIEIP()
			if err != nil {
				log.Errorf("iVM.GetIEIP error: %v", err)
				return false, nil
			}
			if eip.GetGlobalId() == iVM.GetGlobalId() || eip.GetGlobalId() == eip.GetIpAddr() {
				log.Errorf("wait public ip convert to eip (%s)...", eip.GetGlobalId())
				return false, nil
			}
			_, err = db.Update(publicIp, func() error {
				publicIp.ExternalId = eip.GetGlobalId()
				publicIp.IpAddr = eip.GetIpAddr()
				publicIp.Bandwidth = eip.GetBandwidth()
				publicIp.Mode = api.EIP_MODE_STANDALONE_EIP
				return nil
			})
			return true, err
		})
		if err != nil {
			return nil, errors.Wrap(err, "cloudprovider.Wait")
		}
		return nil, nil
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestSetAutoRenewInstance(ctx context.Context, userCred mcclient.TokenCredential, guest *models.SGuest, input api.GuestAutoRenewInput, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		iVM, err := guest.GetIVM(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "guest.GetIVM")
		}
		bc, err := billing.ParseBillingCycle(input.Duration)
		if err != nil {
			return nil, errors.Wrapf(err, "billing.ParseBillingCycle")
		}
		bc.AutoRenew = input.AutoRenew
		err = iVM.SetAutoRenew(bc)
		if err != nil {
			return nil, errors.Wrap(err, "iVM.SetAutoRenew")
		}

		return nil, guest.SetAutoRenew(input.AutoRenew)
	})
	return nil
}

func (self *SManagedVirtualizedGuestDriver) RequestRemoteUpdate(ctx context.Context, guest *models.SGuest, userCred mcclient.TokenCredential, replaceTags bool) error {
	// nil ops
	iVM, err := guest.GetIVM(ctx)
	if err != nil {
		return errors.Wrap(err, "guest.GetIVM")
	}

	err = func() error {
		oldTags, err := iVM.GetTags()
		if err != nil {
			if errors.Cause(err) == cloudprovider.ErrNotSupported || errors.Cause(err) == cloudprovider.ErrNotImplemented {
				return nil
			}
			return errors.Wrap(err, "iVM.GetTags()")
		}
		tags, err := guest.GetAllUserMetadata()
		if err != nil {
			return errors.Wrapf(err, "GetAllUserMetadata")
		}
		tagsUpdateInfo := cloudprovider.TagsUpdateInfo{OldTags: oldTags, NewTags: tags}

		host, _ := guest.GetHost()
		err = cloudprovider.SetTags(ctx, iVM, host.ManagerId, tags, replaceTags)
		if err != nil {
			if errors.Cause(err) == cloudprovider.ErrNotSupported || errors.Cause(err) == cloudprovider.ErrNotImplemented {
				return nil
			}
			logclient.AddSimpleActionLog(guest, logclient.ACT_UPDATE_TAGS, err, userCred, false)
			return errors.Wrap(err, "iVM.SetTags")
		}
		logclient.AddSimpleActionLog(guest, logclient.ACT_UPDATE_TAGS, tagsUpdateInfo, userCred, true)
		// sync back cloud metadata
		iVM.Refresh()
		guest.SyncOsInfo(ctx, userCred, iVM)
		err = models.SyncVirtualResourceMetadata(ctx, userCred, guest, iVM)
		if err != nil {
			return errors.Wrap(err, "syncVirtualResourceMetadata")
		}
		return nil
	}()
	if err != nil {
		return err
	}

	err = iVM.UpdateVM(ctx, guest.Name)
	if err != nil {
		if errors.Cause(err) != cloudprovider.ErrNotSupported {
			return errors.Wrap(err, "iVM.UpdateVM")
		}
	}

	return nil
}
