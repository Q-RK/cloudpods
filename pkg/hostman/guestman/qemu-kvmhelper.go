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

package guestman

import (
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/sergi/go-diff/diffmatchpatch"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/utils"

	"yunion.io/x/onecloud/pkg/apis"
	api "yunion.io/x/onecloud/pkg/apis/compute"
	"yunion.io/x/onecloud/pkg/hostman/guestman/desc"
	"yunion.io/x/onecloud/pkg/hostman/guestman/qemu"
	qemucerts "yunion.io/x/onecloud/pkg/hostman/guestman/qemu/certs"
	"yunion.io/x/onecloud/pkg/hostman/monitor"
	"yunion.io/x/onecloud/pkg/hostman/options"
	"yunion.io/x/onecloud/pkg/util/procutils"
	"yunion.io/x/onecloud/pkg/util/qemutils"
)

const (
	OS_NAME_LINUX   = qemu.OS_NAME_LINUX
	OS_NAME_WINDOWS = qemu.OS_NAME_WINDOWS
	OS_NAME_MACOS   = qemu.OS_NAME_MACOS
	OS_NAME_ANDROID = qemu.OS_NAME_ANDROID
	OS_NAME_VMWARE  = qemu.OS_NAME_VMWARE
	OS_NAME_CIRROS  = qemu.OS_NAME_CIRROS
	OS_NAME_OPENWRT = qemu.OS_NAME_OPENWRT

	MODE_READLINE = qemu.MODE_READLINE
	MODE_CONTROL  = qemu.MODE_CONTROL

	DISK_DRIVER_VIRTIO = qemu.DISK_DRIVER_VIRTIO
	DISK_DRIVER_SCSI   = qemu.DISK_DRIVER_SCSI
	DISK_DRIVER_PVSCSI = qemu.DISK_DRIVER_PVSCSI
	DISK_DRIVER_IDE    = qemu.DISK_DRIVER_IDE
	DISK_DRIVER_SATA   = qemu.DISK_DRIVER_SATA
)

const guestLauncher = `
#!/usr/bin/env python
import sys
import os
import time
import subprocess

with open(os.devnull, 'w')  as FNULL:
    try:
        cmd = subprocess.check_output(['bash', '%s'], stderr=FNULL).split()
    except BaseException as e:
        sys.stderr.write('%%s' %% e)
        sys.exit(1)

pid = os.fork()
if pid < 0:
    sys.stderr.write('failed fork child process')
    sys.exit(1)

if pid > 0:
    status_encoded = os.waitpid(pid, 0)[1]
    sys.exit((status_encoded>>8) & 0xff)
else:
    os.setsid()
    os.chdir('/')

    pid = os.fork()
    if pid < 0:
        sys.stderr.write('failed fork child process')
        sys.exit(1)

    if pid > 0:
        sys.stdout.write('%%d' %% pid)
        sys.exit(0)
    else:
        devnull = os.open('/dev/null', os.O_RDWR)
        os.dup2(devnull, 0)
        os.close(devnull)

        logfd = os.open('%s', os.O_RDWR|os.O_CREAT|os.O_APPEND)
        os.dup2(logfd, 1)
        os.dup2(logfd, 2)
        os.write(logfd, '%%s Run command: %%s\n' %% (time.strftime('%%Y-%%m-%%d %%H:%%M:%%S', time.localtime()), cmd))
        os.close(logfd)

        if os.execv(cmd[0], cmd) < 0:
            sys.stderr.write('exec error')
            sys.exit(1)
`

func (s *SKVMGuestInstance) IsKvmSupport() bool {
	return s.manager.GetHost().IsKvmSupport()
}

func (s *SKVMGuestInstance) IsEnabledNestedVirt() bool {
	return s.manager.GetHost().IsNestedVirtualization()
}

func (s *SKVMGuestInstance) GetKernelVersion() string {
	return s.manager.host.GetKernelVersion()
}

func (s *SKVMGuestInstance) CpuMax() (uint, error) {
	cpuMax, ok := s.manager.qemuMachineCpuMax[s.Desc.Machine]
	if !ok {
		return 0, errors.Errorf("unsupported cpu max for qemu machine: %s", s.Desc.Machine)
	}
	return cpuMax, nil
}

func (s *SKVMGuestInstance) IsVdiSpice() bool {
	return s.Desc.Vdi == "spice"
}

func (s *SKVMGuestInstance) GetOsName() string {
	if osName, ok := s.Desc.Metadata["os_name"]; ok {
		return osName
	}
	return OS_NAME_LINUX
}

func (s *SKVMGuestInstance) disableUsbKbd() bool {
	return s.Desc.Metadata["disable_usb_kbd"] == "true"
}

func (s *SKVMGuestInstance) getOsDistribution() string {
	return s.Desc.Metadata["os_distribution"]
}

func (s *SKVMGuestInstance) getOsVersion() string {
	return s.Desc.Metadata["os_version"]
}

func (s *SKVMGuestInstance) pciInitialized() bool {
	return len(s.Desc.PCIControllers) > 0
}

func (s *SKVMGuestInstance) hasPcieExtendBus() bool {
	return s.Desc.Metadata["__pcie_extend_bus"] == "true"
}

func (s *SKVMGuestInstance) setPcieExtendBus() {
	s.Desc.Metadata["__pcie_extend_bus"] = "true"
}

func (s *SKVMGuestInstance) getUsbControllerType() string {
	usbContType := s.Desc.Metadata["usb_controller_type"]
	if usbContType == "usb-ehci" {
		return usbContType
	} else {
		return "qemu-xhci"
	}
}

// is windows prioer to windows server 2003
func (s *SKVMGuestInstance) IsOldWindows() bool {
	if s.GetOsName() == OS_NAME_WINDOWS {
		ver := s.getOsVersion()
		if len(ver) > 1 && ver[0:2] == "5." {
			return true
		}
	}
	return false
}

func (s *SKVMGuestInstance) isWindows10() bool {
	if s.GetOsName() == OS_NAME_WINDOWS {
		distro := s.getOsDistribution()
		if strings.Contains(strings.ToLower(distro), "windows 10") {
			return true
		}
	}
	return false
}

func (s *SKVMGuestInstance) isMemcleanEnabled() bool {
	return s.Desc.Metadata["enable_memclean"] == "true"
}

func (s *SKVMGuestInstance) getMachine() string {
	machine := s.Desc.Machine
	if machine == "" {
		machine = api.VM_MACHINE_TYPE_PC
	}
	return machine
}

func (s *SKVMGuestInstance) getBios() string {
	bios := s.Desc.Bios
	if bios == "" {
		bios = "bios"
	}
	return bios
}

func (s *SKVMGuestInstance) isQ35() bool {
	return s.getMachine() == api.VM_MACHINE_TYPE_Q35
}

func (s *SKVMGuestInstance) isVirt() bool {
	return s.getMachine() == api.VM_MACHINE_TYPE_ARM_VIRT
}

func (s *SKVMGuestInstance) isPcie() bool {
	return utils.IsInStringArray(s.getMachine(),
		[]string{api.VM_MACHINE_TYPE_Q35, api.VM_MACHINE_TYPE_ARM_VIRT})
}

func (s *SKVMGuestInstance) GetVdiProtocol() string {
	vdi := s.Desc.Vdi
	if vdi == "" {
		vdi = "vnc"
	}
	return vdi
}

func (s *SKVMGuestInstance) GetPciBus() string {
	if s.isQ35() || s.isVirt() {
		return "pcie.0"
	} else {
		return "pci.0"
	}
}

func (s *SKVMGuestInstance) disableIsaSerialDev() bool {
	return s.Desc.Metadata["disable_isa_serial"] == "true"
}

func (s *SKVMGuestInstance) disablePvpanicDev() bool {
	return s.Desc.Metadata["disable_pvpanic"] == "true"
}

func (s *SKVMGuestInstance) getNicUpScriptPath(nic *desc.SGuestNetwork) string {
	dev := s.manager.GetHost().GetBridgeDev(nic.Bridge)
	return path.Join(s.HomeDir(), fmt.Sprintf("if-up-%s-%s.sh", dev.Bridge(), nic.Ifname))
}

func (s *SKVMGuestInstance) getNicDownScriptPath(nic *desc.SGuestNetwork) string {
	dev := s.manager.GetHost().GetBridgeDev(nic.Bridge)
	return path.Join(s.HomeDir(), fmt.Sprintf("if-down-%s-%s.sh", dev.Bridge(), nic.Ifname))
}

func (s *SKVMGuestInstance) generateNicScripts(nic *desc.SGuestNetwork) error {
	bridge := nic.Bridge
	dev := s.manager.GetHost().GetBridgeDev(bridge)
	if dev == nil {
		return fmt.Errorf("Can't find bridge %s", bridge)
	}
	isVolatileHost := s.IsSlave() || s.IsMigratingDestGuest()
	if err := dev.GenerateIfupScripts(s.getNicUpScriptPath(nic), nic, isVolatileHost); err != nil {
		return errors.Wrap(err, "GenerateIfupScripts")
	}
	if err := dev.GenerateIfdownScripts(s.getNicDownScriptPath(nic), nic, isVolatileHost); err != nil {
		return errors.Wrap(err, "GenerateIfdownScripts")
	}
	return nil
}

func (s *SKVMGuestInstance) getNicDeviceModel(name string) string {
	return qemu.GetNicDeviceModel(name)
}

func (s *SKVMGuestInstance) extraOptions() string {
	cmd := " "
	for k, v := range s.Desc.ExtraOptions {
		switch jsonV := v.(type) {
		case *jsonutils.JSONArray:
			for i := 0; i < jsonV.Size(); i++ {
				vAtI, _ := jsonV.GetAt(i)
				cmd += fmt.Sprintf(" -%s %s", k, vAtI.String())
			}
		default:
			cmd += fmt.Sprintf(" -%s %s", k, v.String())
		}
	}
	return cmd
}

func (s *SKVMGuestInstance) generateStartScript(data *jsonutils.JSONDict) (string, error) {
	// initial data
	var input = &qemu.GenerateStartOptionsInput{
		GuestDesc:            s.Desc,
		OsName:               s.GetOsName(),
		OVNIntegrationBridge: options.HostOptions.OvnIntegrationBridge,
		HomeDir:              s.HomeDir(),
		HugepagesEnabled:     s.manager.host.IsHugepagesEnabled(),
		EnableMemfd:          s.isMemcleanEnabled(),
		PidFilePath:          s.GetPidFilePath(),
	}

	if data.Contains("encrypt_key") {
		key, _ := data.GetString("encrypt_key")
		if err := s.saveEncryptKeyFile(key); err != nil {
			return "", errors.Wrap(err, "save encrypt key file")
		}
		input.EncryptKeyPath = s.getEncryptKeyPath()
	}

	cmd := ""

	// inject vncPort
	vncPort, _ := data.Int("vnc_port")
	input.VNCPort = uint(vncPort)

	// inject qemu version and arch
	qemuVersion := options.HostOptions.DefaultQemuVersion
	if data.Contains("qemu_version") {
		qemuVersion, _ = data.GetString("qemu_version")
	}
	if qemuVersion == "latest" {
		qemuVersion = ""
	}
	input.QemuVersion = qemu.Version(qemuVersion)
	// inject qemu arch
	if s.manager.host.IsAarch64() {
		input.QemuArch = qemu.Arch_aarch64
	} else {
		input.QemuArch = qemu.Arch_x86_64
	}

	for _, nic := range s.Desc.Nics {
		if nic.Driver == api.NETWORK_DRIVER_VFIO {
			continue
		}
		downscript := s.getNicDownScriptPath(nic)
		cmd += fmt.Sprintf("%s %s\n", downscript, nic.Ifname)
	}

	if input.HugepagesEnabled {
		cmd += fmt.Sprintf("mkdir -p /dev/hugepages/%s\n", s.Desc.Uuid)
		cmd += fmt.Sprintf("mount -t hugetlbfs -o pagesize=%dK,size=%dM hugetlbfs-%s /dev/hugepages/%s\n",
			s.manager.host.HugepageSizeKb(), s.Desc.Mem, s.Desc.Uuid, s.Desc.Uuid)
	}

	cmd += "sleep 1\n"
	cmd += fmt.Sprintf("echo %d > %s\n", input.VNCPort, s.GetVncFilePath())

	diskScripts, err := s.generateDiskSetupScripts(s.Desc.Disks)
	if err != nil {
		return "", errors.Wrap(err, "generateDiskSetupScripts")
	}
	cmd += diskScripts

	sriovInitScripts, err := s.generateSRIOVInitScripts()
	if err != nil {
		return "", errors.Wrap(err, "generateSRIOVInitScripts")
	}
	cmd += sriovInitScripts

	cmd += fmt.Sprintf("STATE_FILE=`ls -d %s* | head -n 1`\n", s.getStateFilePathRootPrefix())
	cmd += fmt.Sprintf("PID_FILE=%s\n", input.PidFilePath)

	var qemuCmd = qemutils.GetQemu(string(input.QemuVersion))
	if len(qemuCmd) == 0 {
		qemuCmd = qemutils.GetQemu("")
	}

	cmd += fmt.Sprintf("DEFAULT_QEMU_CMD='%s'\n", qemuCmd)
	/*
	 * cmd += "if [ -n \"$STATE_FILE\" ]; then\n"
	 * cmd += "    QEMU_VER=`echo $STATE_FILE" +
	 * 	` | grep -o '_[[:digit:]]\+\.[[:digit:]]\+.*'` + "`\n"
	 * cmd += "    QEMU_CMD=\"qemu-system-x86_64\"\n"
	 * cmd += "    QEMU_LOCAL_PATH=\"/usr/local/bin/$QEMU_CMD\"\n"
	 * cmd += "    QEMU_LOCAL_PATH_VER=\"/usr/local/qemu-$QEMU_VER/bin/$QEMU_CMD\"\n"
	 * cmd += "    QEMU_BIN_PATH=\"/usr/bin/$QEMU_CMD\"\n"
	 * cmd += "    if [ -f \"$QEMU_LOCAL_PATH_VER\" ]; then\n"
	 * cmd += "        QEMU_CMD=$QEMU_LOCAL_PATH_VER\n"
	 * cmd += "    elif [ -f \"$QEMU_LOCAL_PATH\" ]; then\n"
	 * cmd += "        QEMU_CMD=$QEMU_LOCAL_PATH\n"
	 * cmd += "    elif [ -f \"$QEMU_BIN_PATH\" ]; then\n"
	 * cmd += "        QEMU_CMD=$QEMU_BIN_PATH\n"
	 * cmd += "    fi\n"
	 * cmd += "else\n"
	 * cmd += "    QEMU_CMD=$DEFAULT_QEMU_CMD\n"
	 * cmd += "fi\n"
	 */
	cmd += "QEMU_CMD=$DEFAULT_QEMU_CMD\n"
	if s.IsKvmSupport() && !options.HostOptions.DisableKVM {
		cmd += "QEMU_CMD_KVM_ARG=-enable-kvm\n"
	} else if utils.IsInStringArray(s.manager.host.GetCpuArchitecture(), apis.ARCH_X86) {
		// -no-kvm仅x86适用，且将在qemu 5.2之后移除
		// https://gitlab.com/qemu-project/qemu/-/blob/master/docs/about/removed-features.rst
		cmd += "QEMU_CMD_KVM_ARG=-no-kvm\n"
	} else {
		cmd += "QEMU_CMD_KVM_ARG=\n"
	}
	// cmd += "fi\n"
	cmd += `
function nic_speed() {
    $QEMU_CMD $QEMU_CMD_KVM_ARG -device virtio-net-pci,help 2>&1 | grep -q "\<speed="
    if [ "$?" -eq "0" ]; then
        echo ",speed=$1"
    fi
}

function nic_mtu() {
    local bridge="$1"; shift

    $QEMU_CMD $QEMU_CMD_KVM_ARG -device virtio-net-pci,help 2>&1 | grep -q '\<host_mtu='
    if [ "$?" -eq "0" ]; then
        local origmtu="$(<"/sys/class/net/$bridge/mtu")"
        if [ -n "$origmtu" -a "$origmtu" -gt 576 ]; then
            echo ",host_mtu=$(($origmtu - ` + api.VpcOvnEncapCostStr() + `))"
        fi
    fi
}
`

	// Generate Start VM script
	cmd += `CMD="$QEMU_CMD $QEMU_CMD_KVM_ARG`

	if options.HostOptions.EnableQemuDebugLog {
		input.EnableLog = true
	}

	// inject monitor
	input.HMPMonitor = &qemu.Monitor{
		Id:   "hmqmon",
		Port: uint(s.GetHmpMonitorPort(int(input.VNCPort))),
		Mode: MODE_READLINE,
	}
	input.QMPMonitor = &qemu.Monitor{
		Id:   "qmqmon",
		Port: uint(s.GetQmpMonitorPort(int(input.VNCPort))),
		Mode: MODE_CONTROL,
	}

	input.EnableUUID = options.HostOptions.EnableVmUuid
	if s.Desc.Bios == qemu.BIOS_UEFI {
		if len(input.OVMFPath) == 0 {
			input.OVMFPath = options.HostOptions.OvmfPath
		}
	}

	// inject usb devices
	if input.QemuArch == qemu.Arch_aarch64 {
		input.Devices = append(input.Devices,
			fmt.Sprintf("usb-tablet,id=input0,bus=%s.0,port=1", s.Desc.Usb.Id),
			fmt.Sprintf("usb-kbd,id=input1,bus=%s.0,port=2", s.Desc.Usb.Id),
		)
		// "qemu-xhci,p2=8,p3=8,id=usb1",
		// "usb-tablet,id=input0,bus=usb1.0,port=1",
		// "usb-kbd,id=input1,bus=usb1.0,port=2",
		// "virtio-gpu-pci,id=video1,max_outputs=1",
	} else {
		if !utils.IsInStringArray(s.getOsDistribution(), []string{OS_NAME_OPENWRT, OS_NAME_CIRROS}) &&
			!s.IsOldWindows() && !s.isWindows10() &&
			!s.disableUsbKbd() {
			input.Devices = append(input.Devices, "usb-kbd")
		}
		if input.OsName == OS_NAME_ANDROID {
			input.Devices = append(input.Devices, "usb-mouse")
		} else if !s.IsOldWindows() {
			input.Devices = append(input.Devices, "usb-tablet")
		}
	}

	// inject spice and vnc display
	input.IsVdiSpice = s.IsVdiSpice()
	input.SpicePort = uint(5900 + vncPort)
	input.VNCPassword = options.HostOptions.SetVncPassword

	input.IsKVMSupport = s.IsKvmSupport()
	input.ExtraOptions = append(input.ExtraOptions, s.extraOptions())

	if jsonutils.QueryBoolean(data, "need_migrate", false) {
		input.NeedMigrate = true
		input.LiveMigratePort = uint(*s.LiveMigrateDestPort)
		if jsonutils.QueryBoolean(data, "live_migrate_use_tls", false) {
			s.LiveMigrateUseTls = true
			input.LiveMigrateUseTLS = true
		}
	} else if s.Desc.IsSlave {
		input.LiveMigratePort = uint(*s.LiveMigrateDestPort)
	}

	qemuOpts, err := qemu.GenerateStartOptions(input)
	if err != nil {
		return "", errors.Wrap(err, "GenerateStartCommand")
	}
	cmd = fmt.Sprintf("%s %s", cmd, qemuOpts)
	cmd += "\"\n"

	cmd += `
if [ ! -z "$STATE_FILE" ] && [ -d "$STATE_FILE" ] && [ -f "$STATE_FILE/content" ]; then
    CMD="$CMD --incoming \"exec: cat $STATE_FILE/content\""
elif [ ! -z "$STATE_FILE" ] && [ -f "$STATE_FILE" ]; then
    CMD="$CMD --incoming \"exec: cat $STATE_FILE\""
fi
echo $CMD`

	return cmd, nil
}

func (s *SKVMGuestInstance) parseCmdline(input string) (*qemutils.Cmdline, []qemutils.Option, error) {
	cl, err := qemutils.NewCmdline(input)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "NewCmdline %q", input)
	}
	filterOpts := make([]qemutils.Option, 0)
	// filter migrate and other option include dynamic port
	cl.FilterOption(func(o qemutils.Option) bool {
		switch o.Key {
		case "incoming":
			if strings.HasPrefix(o.Value, "tcp:") || strings.HasPrefix(o.Value, "defer") {
				filterOpts = append(filterOpts, o)
				return true
			}
		case "vnc", "spice", "daemonize":
			filterOpts = append(filterOpts, o)
			return true
		case "chardev":
			valsMatch := []string{
				"socket,id=hmqmondev",
				"socket,id=hmpmondev",
				"socket,id=qmqmondev",
				"socket,id=qmpmondev",
			}
			for _, valM := range valsMatch {
				if strings.HasPrefix(o.Value, valM) {
					filterOpts = append(filterOpts, o)
					return true
				}
			}
		}
		return false
	})
	return cl, filterOpts, nil
}

func (s *SKVMGuestInstance) _unifyMigrateQemuCmdline(cur string, src string) string {
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(cur, src, false)

	log.Debugf("unify migrate qemu cmdline diffs: %s", jsonutils.Marshal(diffs).PrettyString())

	// make patch
	patch := dmp.PatchMake(cur, diffs)

	// apply patch
	newStr, _ := dmp.PatchApply(patch, cur)
	return newStr
}

func (s *SKVMGuestInstance) unifyMigrateQemuCmdline(cur string, src string) (string, error) {
	curCl, curFilterOpts, err := s.parseCmdline(cur)
	if err != nil {
		return "", errors.Wrapf(err, "parseCmdline current %q", cur)
	}
	srcCl, _, err := s.parseCmdline(src)
	if err != nil {
		return "", errors.Wrapf(err, "parseCmdline source %q", src)
	}
	unifyStr := s._unifyMigrateQemuCmdline(curCl.ToString(), srcCl.ToString())
	unifyCl, _, err := s.parseCmdline(unifyStr)
	if err != nil {
		return "", errors.Wrapf(err, "parseCmdline unitfy %q", unifyStr)
	}
	unifyCl.AddOption(curFilterOpts...)
	return unifyCl.ToString(), nil
}

func (s *SKVMGuestInstance) generateStopScript(data *jsonutils.JSONDict) string {
	var (
		uuid = s.Desc.Uuid
		nics = s.Desc.Nics
	)

	cmd := ""
	cmd += fmt.Sprintf("VNC_FILE=%s\n", s.GetVncFilePath())
	cmd += fmt.Sprintf("PID_FILE=%s\n", s.GetPidFilePath())
	cmd += "if [ \"$1\" != \"--force\" ] && [ -f $VNC_FILE ]; then\n"
	cmd += "  VNC=`cat $VNC_FILE`\n"

	// TODO, replace with qmp monitor
	cmd += fmt.Sprintf("  MON=$(($VNC + %d))\n", MONITOR_PORT_BASE)
	cmd += "  echo quit | nc -w 1 127.0.0.1 $MON > /dev/null\n"
	cmd += "  sleep 1\n"
	cmd += "  echo \"Remove VNC $VNC_FILE\"\n"
	cmd += "  rm -f $VNC_FILE\n"
	cmd += "fi\n"
	cmd += "if [ -f $PID_FILE ]; then\n"
	cmd += "  PID=`cat $PID_FILE`\n"
	cmd += "  ps -p $PID > /dev/null\n"
	cmd += "  if [ $? -eq 0 ]; then\n"
	cmd += "    echo \"Kill process $PID\"\n"
	cmd += "    kill -9 $PID > /dev/null 2>&1\n"
	cmd += "  fi\n"
	cmd += "  echo \"Remove PID $PID_FILE\"\n"
	cmd += "  rm -f $PID_FILE\n"
	cmd += "fi\n"

	cmd += fmt.Sprintf("for d in $(ls -d /dev/hugepages/%s*)\n", uuid)
	cmd += fmt.Sprintf("do\n")
	cmd += fmt.Sprintf("  if [ -d $d ]; then\n")
	cmd += fmt.Sprintf("    umount $d\n")
	cmd += fmt.Sprintf("    rm -rf $d\n")
	cmd += fmt.Sprintf("  fi\n")
	cmd += fmt.Sprintf("done\n")

	for _, nic := range nics {
		if nic.Driver == api.NETWORK_DRIVER_VFIO {
			continue
		}
		downscript := s.getNicDownScriptPath(nic)
		cmd += fmt.Sprintf("%s %s\n", downscript, nic.Ifname)
	}
	return cmd
}

func (s *SKVMGuestInstance) presendArpForNic(nic *desc.SGuestNetwork) {
	ifi, err := net.InterfaceByName(nic.Ifname)
	if err != nil {
		log.Errorf("InterfaceByName error %s", nic.Ifname)
		return
	}

	cli, err := arp.Dial(ifi)
	if err != nil {
		log.Errorf("arp Dial error %s", err)
		return
	}
	defer cli.Close()

	var (
		sSrcMac   = nic.Mac
		sScrIp    = nic.Ip
		srcIp     = net.ParseIP(sScrIp)
		dstMac, _ = net.ParseMAC("00:00:00:00:00:00")
		dstIp     = net.ParseIP("255.255.255.255")
	)
	srcMac, err := net.ParseMAC(sSrcMac)
	if err != nil {
		log.Errorf("Send arp parse mac error: %s", err)
		return
	}

	pkt, err := arp.NewPacket(arp.OperationRequest, srcMac, srcIp, dstMac, dstIp)
	if err != nil {
		log.Errorf("New arp packet error %s", err)
		return
	}
	if err := cli.WriteTo(pkt, ethernet.Broadcast); err != nil {
		log.Errorf("Send arp packet error %s ", err)
		return
	}
}

func (s *SKVMGuestInstance) StartPresendArp() {
	go func() {
		for i := 0; i < 5; i++ {
			for _, nic := range s.Desc.Nics {
				s.presendArpForNic(nic)
			}
			time.Sleep(1 * time.Second)
		}
	}()
}

func (s *SKVMGuestInstance) getPKIDirPath() string {
	return path.Join(s.HomeDir(), "pki")
}

func (s *SKVMGuestInstance) makePKIDir() error {
	output, err := procutils.NewCommand("mkdir", "-p", s.getPKIDirPath()).Output()
	if err != nil {
		return errors.Wrapf(err, "mkdir %s failed: %s", s.getPKIDirPath(), output)
	}
	return nil
}

func (s *SKVMGuestInstance) PrepareMigrateCerts() (map[string]string, error) {
	pkiDir := s.getPKIDirPath()
	if err := s.makePKIDir(); err != nil {
		return nil, errors.Wrap(err, "make pki dir")
	}
	tree, err := qemucerts.GetDefaultCertList().AsMap().CertTree()
	if err != nil {
		return nil, errors.Wrap(err, "construct cert tree")
	}
	if err := tree.CreateTree(pkiDir); err != nil {
		return nil, errors.Wrap(err, "create certs")
	}
	return qemucerts.FetchDefaultCerts(pkiDir)
}

func (s *SKVMGuestInstance) WriteMigrateCerts(certs map[string]string) error {
	pkiDir := s.getPKIDirPath()
	if err := s.makePKIDir(); err != nil {
		return errors.Wrap(err, "make pki dir")
	}
	if err := qemucerts.CreateByMap(pkiDir, certs); err != nil {
		return errors.Wrapf(err, "create by map %#v", certs)
	}
	return nil
}

func (s *SKVMGuestInstance) startMemCleaner() error {
	err := procutils.NewRemoteCommandAsFarAsPossible(
		options.HostOptions.BinaryMemcleanPath,
		"--pid", strconv.Itoa(s.GetPid()),
		"--mem-size", strconv.FormatInt(s.Desc.Mem*1024*1024, 10),
		"--log-dir", s.HomeDir(),
	).Run()
	if err != nil {
		log.Errorf("failed start memcleaner: %s", err)
		return errors.Wrap(err, "start memclean")
	}
	return nil
}

func (s *SKVMGuestInstance) HasGpu() bool {
	for i := 0; i < len(s.Desc.IsolatedDevices); i++ {
		if s.Desc.IsolatedDevices[i].DevType != api.USB_TYPE {
			return true
		}
	}
	return false
}

func (s *SKVMGuestInstance) gpusHasVga() bool {
	manager := s.manager.GetHost().GetIsolatedDeviceManager()
	for i := 0; i < len(s.Desc.IsolatedDevices); i++ {
		dev := manager.GetDeviceByAddr(s.Desc.IsolatedDevices[i].Addr)
		if dev.GetDeviceType() == api.GPU_VGA_TYPE {
			return true
		}
	}
	return false
}

func (s *SKVMGuestInstance) initCpuDesc() error {
	s.fixGuestMachineType()
	cpuDesc, err := s.archMan.GenerateCpuDesc(uint(s.Desc.Cpu), s)
	if err != nil {
		return err
	}
	s.Desc.CpuDesc = cpuDesc
	return nil
}

func (s *SKVMGuestInstance) initMemDesc(memSizeMB int64) {
	s.Desc.MemDesc = s.archMan.GenerateMemDesc()
	s.Desc.MemDesc.SizeMB = memSizeMB
	if s.manager.host.IsHugepagesEnabled() {
		s.Desc.MemDesc.Mem = desc.NewObject("memory-backend-file", "mem")
		s.Desc.MemDesc.Mem.Options = map[string]string{
			"mem-path": fmt.Sprintf("/dev/hugepages/%s", s.Desc.Uuid),
			"size":     fmt.Sprintf("%dM", memSizeMB),
			"share":    "on", "prealloc": "on",
		}
	} else if s.isMemcleanEnabled() {
		s.Desc.MemDesc.Mem = desc.NewObject("memory-backend-memfd", "mem")
		s.Desc.MemDesc.Mem.Options = map[string]string{
			"size":  fmt.Sprintf("%dM", memSizeMB),
			"share": "on", "prealloc": "on",
		}
	} else {
		s.Desc.MemDesc.Mem = desc.NewObject("memory-backend-ram", "mem")
		s.Desc.MemDesc.Mem.Options = map[string]string{
			"size": fmt.Sprintf("%dM", memSizeMB),
		}
	}
}

func (s *SKVMGuestInstance) initMemDescFromMemoryInfo(memoryDevicesInfoList []monitor.MemoryDeviceInfo) error {
	var objType string
	if s.manager.host.IsHugepagesEnabled() {
		objType = "memory-backend-file"
	} else if s.isMemcleanEnabled() {
		objType = "memory-backend-memfd"
	} else {
		objType = "memory-backend-ram"
	}

	memSize := s.Desc.Mem
	memSlots := make([]*desc.SMemSlot, 0)
	for i := 0; i < len(memoryDevicesInfoList); i++ {
		if memoryDevicesInfoList[i].Type != "dimm" || memoryDevicesInfoList[i].Data.ID == nil {
			return errors.Errorf("unsupported memory device type %s", memoryDevicesInfoList[i].Type)
		}
		memSize -= (memoryDevicesInfoList[i].Data.Size / 1024 / 1024)
		memSlots = append(memSlots, &desc.SMemSlot{
			SizeMB: memoryDevicesInfoList[i].Data.Size / 1024 / 1024,
			MemObj: desc.NewObject(objType, path.Base(memoryDevicesInfoList[i].Data.Memdev)),
			MemDev: &desc.SMemDevice{
				Type: "pc-dimm", Id: *memoryDevicesInfoList[i].Data.ID,
			},
		})
	}
	if memSize <= 0 {
		return errors.Errorf("wrong memsize %d", s.Desc.Mem)
	}
	s.initMemDesc(memSize)
	s.Desc.MemDesc.MemSlots = memSlots
	return nil
}

func (s *SKVMGuestInstance) fixGuestMachineType() {
	if s.GetOsName() == OS_NAME_MACOS {
		s.Desc.Machine = api.VM_MACHINE_TYPE_Q35
		s.Desc.Bios = qemu.BIOS_UEFI
	}
	if s.manager.host.IsAarch64() {
		if utils.IsInStringArray(s.Desc.Machine, []string{
			"", api.VM_MACHINE_TYPE_PC, api.VM_MACHINE_TYPE_Q35,
		}) {
			s.Desc.Machine = api.VM_MACHINE_TYPE_ARM_VIRT
		}
		s.Desc.Bios = qemu.BIOS_UEFI
	}
}

func (s *SKVMGuestInstance) initMachineDesc() {
	s.Desc.MachineDesc = s.archMan.GenerateMachineDesc(s.Desc.CpuDesc.Accel)
}

func (s *SKVMGuestInstance) initQgaDesc() {
	s.Desc.Qga = s.archMan.GenerateQgaDesc(path.Join(s.HomeDir(), "qga.sock"))
}

func (s *SKVMGuestInstance) initPvpanicDesc() {
	s.Desc.Pvpanic = s.archMan.GeneratePvpanicDesc()
}

func (s *SKVMGuestInstance) initIsaSerialDesc() {
	if !s.disableIsaSerialDev() {
		s.Desc.IsaSerial = s.archMan.GenerateIsaSerialDesc()
	}
}

func (s *SKVMGuestInstance) getHotPlugPciController() *desc.PCIController {
	for i := 0; i < len(s.Desc.PCIControllers); i++ {
		switch s.Desc.PCIControllers[i].CType {
		case desc.CONTROLLER_TYPE_PCI_ROOT, desc.CONTROLLER_TYPE_PCI_BRIDGE:
			return s.Desc.PCIControllers[i]
		}
	}
	return nil
}
