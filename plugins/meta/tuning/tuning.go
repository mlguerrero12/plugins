// Copyright 2016 CNI authors
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

// This is a "meta-plugin". It reads in its own netconf, it does not create
// any network interface but just changes the network sysctl.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/netlinksafe"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

const (
	defaultDataDir       = "/run/cni/tuning"
	defaultAllowlistDir  = "/etc/cni/tuning/"
	defaultAllowlistFile = "allowlist.conf"
)

// TuningConf represents the network tuning configuration.
type TuningConf struct {
	types.NetConf
	DataDir  string            `json:"dataDir,omitempty"`
	SysCtl   map[string]string `json:"sysctl"`
	Mac      string            `json:"mac,omitempty"`
	Promisc  bool              `json:"promisc,omitempty"`
	Mtu      int               `json:"mtu,omitempty"`
	TxQLen   *int              `json:"txQLen,omitempty"`
	Allmulti *bool             `json:"allmulti,omitempty"`

	RuntimeConfig struct {
		Mac string `json:"mac,omitempty"`
	} `json:"runtimeConfig,omitempty"`
	Args *struct {
		A *IPAMArgs `json:"cni"`
	} `json:"args"`
}

type IPAMArgs struct {
	SysCtl   *map[string]string `json:"sysctl"`
	Mac      *string            `json:"mac,omitempty"`
	Promisc  *bool              `json:"promisc,omitempty"`
	Mtu      *int               `json:"mtu,omitempty"`
	Allmulti *bool              `json:"allmulti,omitempty"`
	TxQLen   *int               `json:"txQLen,omitempty"`
}

// configToRestore will contain interface attributes that should be restored on cmdDel
type configToRestore struct {
	Mac      string `json:"mac,omitempty"`
	Promisc  *bool  `json:"promisc,omitempty"`
	Mtu      int    `json:"mtu,omitempty"`
	Allmulti *bool  `json:"allmulti,omitempty"`
	TxQLen   *int   `json:"txQLen,omitempty"`
}

// MacEnvArgs represents CNI_ARG
type MacEnvArgs struct {
	types.CommonArgs
	MAC types.UnmarshallableString `json:"mac,omitempty"`
}

func parseConf(data []byte, envArgs string) (*TuningConf, error) {
	conf := TuningConf{Promisc: false}
	if err := json.Unmarshal(data, &conf); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	if conf.DataDir == "" {
		conf.DataDir = defaultDataDir
	}

	// Parse custom Mac from both env args
	if envArgs != "" {
		e := MacEnvArgs{}
		err := types.LoadArgs(envArgs, &e)
		if err != nil {
			return nil, err
		}

		if e.MAC != "" {
			conf.Mac = string(e.MAC)
		}
	}

	// Parse custom Mac from RuntimeConfig
	if conf.RuntimeConfig.Mac != "" {
		conf.Mac = conf.RuntimeConfig.Mac
	}

	// Get args
	if conf.Args != nil && conf.Args.A != nil {
		if conf.Args.A.SysCtl != nil {
			for k, v := range *conf.Args.A.SysCtl {
				conf.SysCtl[k] = v
			}
		}

		if conf.Args.A.Mac != nil {
			conf.Mac = *conf.Args.A.Mac
		}

		if conf.Args.A.Promisc != nil {
			conf.Promisc = *conf.Args.A.Promisc
		}

		if conf.Args.A.Mtu != nil {
			conf.Mtu = *conf.Args.A.Mtu
		}

		if conf.Args.A.Allmulti != nil {
			conf.Allmulti = conf.Args.A.Allmulti
		}

		if conf.Args.A.TxQLen != nil {
			conf.TxQLen = conf.Args.A.TxQLen
		}
	}

	return &conf, nil
}

func changeMacAddr(ifName string, newMacAddr string) error {
	addr, err := net.ParseMAC(newMacAddr)
	if err != nil {
		return fmt.Errorf("invalid args %v for MAC addr: %v", newMacAddr, err)
	}

	link, err := netlinksafe.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to get %q: %v", ifName, err)
	}

	return netlink.LinkSetHardwareAddr(link, addr)
}

func updateResultsMacAddr(config *TuningConf, ifName string, newMacAddr string) {
	// Parse previous result.
	if config.PrevResult == nil {
		return
	}

	version.ParsePrevResult(&config.NetConf)
	result, _ := current.NewResultFromResult(config.PrevResult)

	for _, i := range result.Interfaces {
		if i.Name == ifName {
			i.Mac = newMacAddr
		}
	}
	config.PrevResult = result
}

func changePromisc(ifName string, val bool) error {
	link, err := netlinksafe.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to get %q: %v", ifName, err)
	}

	if val {
		return netlink.SetPromiscOn(link)
	}
	return netlink.SetPromiscOff(link)
}

func changeMtu(ifName string, mtu int) error {
	link, err := netlinksafe.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to get %q: %v", ifName, err)
	}
	return netlink.LinkSetMTU(link, mtu)
}

func changeAllmulti(ifName string, val bool) error {
	link, err := netlinksafe.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to get %q: %v", ifName, err)
	}

	if val {
		return netlink.LinkSetAllmulticastOn(link)
	}
	return netlink.LinkSetAllmulticastOff(link)
}

func changeTxQLen(ifName string, txQLen int) error {
	link, err := netlinksafe.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to get %q: %v", ifName, err)
	}
	return netlink.LinkSetTxQLen(link, txQLen)
}

func createBackup(ifName, containerID, backupPath string, tuningConf *TuningConf) error {
	config := configToRestore{}
	link, err := netlinksafe.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to get %q: %v", ifName, err)
	}
	if tuningConf.Mac != "" {
		config.Mac = link.Attrs().HardwareAddr.String()
	}
	if tuningConf.Promisc {
		config.Promisc = new(bool)
		*config.Promisc = (link.Attrs().Promisc != 0)
	}
	if tuningConf.Mtu != 0 {
		config.Mtu = link.Attrs().MTU
	}
	if tuningConf.Allmulti != nil {
		config.Allmulti = new(bool)
		*config.Allmulti = (link.Attrs().RawFlags&unix.IFF_ALLMULTI != 0)
	}
	if tuningConf.TxQLen != nil {
		qlen := link.Attrs().TxQLen
		config.TxQLen = &qlen
	}

	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		if err = os.MkdirAll(backupPath, 0o600); err != nil {
			return fmt.Errorf("failed to create backup directory: %v", err)
		}
	}

	data, err := json.MarshalIndent(config, "", " ")
	if err != nil {
		return fmt.Errorf("failed to marshall data for %q: %v", ifName, err)
	}
	if err = os.WriteFile(path.Join(backupPath, containerID+"_"+ifName+".json"), data, 0o600); err != nil {
		return fmt.Errorf("failed to save file %s.json: %v", ifName, err)
	}

	return nil
}

func restoreBackup(ifName, containerID, backupPath string) error {
	filePath := path.Join(backupPath, containerID+"_"+ifName+".json")

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		// No backup file - nothing to revert
		return nil
	}

	file, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %q: %v", filePath, err)
	}

	config := configToRestore{}
	if err = json.Unmarshal(file, &config); err != nil {
		return nil
	}

	var errStr []string

	_, err = netlinksafe.LinkByName(ifName)
	if err != nil {
		return nil
	}

	if config.Mtu != 0 {
		if err = changeMtu(ifName, config.Mtu); err != nil {
			err = fmt.Errorf("failed to restore MTU: %v", err)
			errStr = append(errStr, err.Error())
		}
	}
	if config.Mac != "" {
		if err = changeMacAddr(ifName, config.Mac); err != nil {
			err = fmt.Errorf("failed to restore MAC address: %v", err)
			errStr = append(errStr, err.Error())
		}
	}
	if config.Promisc != nil {
		if err = changePromisc(ifName, *config.Promisc); err != nil {
			err = fmt.Errorf("failed to restore promiscuous mode: %v", err)
			errStr = append(errStr, err.Error())
		}
	}
	if config.Allmulti != nil {
		if err = changeAllmulti(ifName, *config.Allmulti); err != nil {
			err = fmt.Errorf("failed to restore all-multicast mode: %v", err)
			errStr = append(errStr, err.Error())
		}
	}

	if config.TxQLen != nil {
		if err = changeTxQLen(ifName, *config.TxQLen); err != nil {
			err = fmt.Errorf("failed to restore transmit queue length: %v", err)
			errStr = append(errStr, err.Error())
		}
	}

	if len(errStr) > 0 {
		return errors.New(strings.Join(errStr, "; "))
	}

	if err = os.Remove(filePath); err != nil {
		return fmt.Errorf("failed to remove file %v: %v", filePath, err)
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	if err := validateSysctlConflictingKeys(args.StdinData); err != nil {
		return err
	}
	tuningConf, err := parseConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	if err = validateSysctlConf(tuningConf); err != nil {
		return err
	}

	if err = validateArgs(args); err != nil {
		return err
	}

	// Parse previous result.
	if tuningConf.RawPrevResult == nil {
		return fmt.Errorf("Required prevResult missing")
	}

	if err := version.ParsePrevResult(&tuningConf.NetConf); err != nil {
		return err
	}

	// The directory /proc/sys/net is per network namespace. Enter in the
	// network namespace before writing on it.

	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		for key, value := range tuningConf.SysCtl {
			fileName, err := getSysctlFilename(key, args.IfName)
			if err != nil {
				return err
			}

			content := []byte(value)
			err = os.WriteFile(fileName, content, 0o644)
			if err != nil {
				return err
			}
		}

		if tuningConf.Mac != "" || tuningConf.Mtu != 0 || tuningConf.Promisc || tuningConf.Allmulti != nil || tuningConf.TxQLen != nil {
			if err = createBackup(args.IfName, args.ContainerID, tuningConf.DataDir, tuningConf); err != nil {
				return err
			}
		}

		if tuningConf.Mac != "" {
			if err = changeMacAddr(args.IfName, tuningConf.Mac); err != nil {
				return err
			}

			updateResultsMacAddr(tuningConf, args.IfName, tuningConf.Mac)
		}

		if tuningConf.Promisc {
			if err = changePromisc(args.IfName, true); err != nil {
				return err
			}
		}

		if tuningConf.Mtu != 0 {
			if err = changeMtu(args.IfName, tuningConf.Mtu); err != nil {
				return err
			}
		}

		if tuningConf.Allmulti != nil {
			if err = changeAllmulti(args.IfName, *tuningConf.Allmulti); err != nil {
				return err
			}
		}

		if tuningConf.TxQLen != nil {
			if err = changeTxQLen(args.IfName, *tuningConf.TxQLen); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return types.PrintResult(tuningConf.PrevResult, tuningConf.CNIVersion)
}

// cmdDel will restore NIC attributes to the original ones when called
func cmdDel(args *skel.CmdArgs) error {
	tuningConf, err := parseConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		// MAC address, MTU, promiscuous and all-multicast mode settings will be restored
		return restoreBackup(args.IfName, args.ContainerID, tuningConf.DataDir)
	})
	return nil
}

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:   cmdAdd,
		Check: cmdCheck,
		Del:   cmdDel,
		/* FIXME GC */
		/* FIXME Status */
	}, version.All, bv.BuildString("tuning"))
}

func cmdCheck(args *skel.CmdArgs) error {
	if err := validateSysctlConflictingKeys(args.StdinData); err != nil {
		return err
	}
	tuningConf, err := parseConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	// Parse previous result.
	if tuningConf.RawPrevResult == nil {
		return fmt.Errorf("Required prevResult missing")
	}

	if err := version.ParsePrevResult(&tuningConf.NetConf); err != nil {
		return err
	}

	_, err = current.NewResultFromResult(tuningConf.PrevResult)
	if err != nil {
		return err
	}

	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		// Check each configured value vs what's currently in the container
		for key, confValue := range tuningConf.SysCtl {
			fileName, err := getSysctlFilename(key, args.IfName)
			if err != nil {
				return err
			}

			contents, err := os.ReadFile(fileName)
			if err != nil {
				return err
			}
			curValue := strings.TrimSuffix(string(contents), "\n")
			if confValue != curValue {
				return fmt.Errorf("Error: Tuning configured value of %s is %s, current value is %s", fileName, confValue, curValue)
			}
		}

		link, err := netlinksafe.LinkByName(args.IfName)
		if err != nil {
			return fmt.Errorf("Cannot find container link %v", args.IfName)
		}

		if tuningConf.Mac != "" {
			if tuningConf.Mac != link.Attrs().HardwareAddr.String() {
				return fmt.Errorf("Error: Tuning configured Ethernet of %s is %s, current value is %s",
					args.IfName, tuningConf.Mac, link.Attrs().HardwareAddr)
			}
		}

		if tuningConf.Promisc {
			if link.Attrs().Promisc == 0 {
				return fmt.Errorf("Error: Tuning link %s configured promisc is %v, current value is %d",
					args.IfName, tuningConf.Promisc, link.Attrs().Promisc)
			}
		} else {
			if link.Attrs().Promisc != 0 {
				return fmt.Errorf("Error: Tuning link %s configured promisc is %v, current value is %d",
					args.IfName, tuningConf.Promisc, link.Attrs().Promisc)
			}
		}

		if tuningConf.Mtu != 0 {
			if tuningConf.Mtu != link.Attrs().MTU {
				return fmt.Errorf("Error: Tuning configured MTU of %s is %d, current value is %d",
					args.IfName, tuningConf.Mtu, link.Attrs().MTU)
			}
		}

		if tuningConf.Allmulti != nil {
			allmulti := (link.Attrs().RawFlags&unix.IFF_ALLMULTI != 0)
			if allmulti != *tuningConf.Allmulti {
				return fmt.Errorf("Error: Tuning configured all-multicast mode of %s is %v, current value is %v",
					args.IfName, tuningConf.Allmulti, allmulti)
			}
		}

		if tuningConf.TxQLen != nil {
			if *tuningConf.TxQLen != link.Attrs().TxQLen {
				return fmt.Errorf("Error: Tuning configured Transmit Queue Length of %s is %d, current value is %d",
					args.IfName, tuningConf.TxQLen, link.Attrs().TxQLen)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// Validate the sysctls in the tuning config are on the sysctl allowlist file.
// Note that if the allowlist file is missing no validation takes place.
func validateSysctlConf(tuningConf *TuningConf) error {
	isPresent, allowlist, err := readAllowlist()
	if err != nil {
		return err
	}
	if !isPresent {
		return nil
	}
	for sysctl := range tuningConf.SysCtl {
		match, err := contains(sysctl, allowlist)
		if err != nil {
			return err
		}
		if !match {
			return fmt.Errorf("Sysctl %s is not allowed. Only the following sysctls are allowed: %+v", sysctl, allowlist)
		}
	}
	return nil
}

// Validate the allowList contains the given sysctl
func contains(sysctl string, allowList []string) (bool, error) {
	for _, allowListElement := range allowList {
		match, err := regexp.MatchString(allowListElement, sysctl)
		if err != nil {
			return false, err
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

// Read the systctl allowlist from file. Return info if the file is present and the read allowList if it is
func readAllowlist() (bool, []string, error) {
	if _, err := os.Stat(filepath.Join(defaultAllowlistDir, defaultAllowlistFile)); os.IsNotExist(err) {
		return false, nil, nil
	}
	dat, err := os.ReadFile(filepath.Join(defaultAllowlistDir, defaultAllowlistFile))
	if err != nil {
		return false, nil, err
	}

	lines := strings.Split(string(dat), "\n")
	allowList := []string{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > 0 {
			allowList = append(allowList, line)
		}
	}
	return true, allowList, nil
}

type sysctlKey string

type sysctlCheck struct {
	SysCtl map[sysctlKey]string `json:"sysctl"`
}

var sysctlDuplicatesMap = map[sysctlKey]interface{}{}

func (d *sysctlKey) UnmarshalText(data []byte) error {
	key := sysctlKey(string(data))
	if _, exists := sysctlDuplicatesMap[key]; exists {
		return errors.New("duplicated sysctl keys are not allowed")
	}
	sysctlDuplicatesMap[key] = ""
	return nil
}

func validateSysctlConflictingKeys(data []byte) error {
	sysctlCheck := sysctlCheck{}
	return json.Unmarshal(data, &sysctlCheck)
}

func validateArgs(args *skel.CmdArgs) error {
	if strings.Contains(args.IfName, string(os.PathSeparator)) {
		return fmt.Errorf("Interface name (%s) contains an invalid character %s", args.IfName, string(os.PathSeparator))
	}
	return nil
}

func getSysctlFilename(key, ifName string) (string, error) {
	key = strings.ReplaceAll(key, ".", string(os.PathSeparator))

	// If the key contains `IFNAME` - substitute it with args.IfName
	// to allow setting sysctls on a particular interface, on which
	// other operations (like mac/mtu setting) are performed
	key = strings.Replace(key, "IFNAME", ifName, 1)

	fileName := filepath.Join("/proc/sys", key)

	// Refuse to modify sysctl parameters that don't belong
	// to the network subsystem.
	if !strings.HasPrefix(fileName, "/proc/sys/net/") {
		return "", fmt.Errorf("invalid net sysctl key: %q", key)
	}

	return fileName, nil
}
