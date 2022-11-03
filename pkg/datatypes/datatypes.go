package datatypes

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	netattachdef "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v3/pkg/types"
)

const (
	SriovResourceKey   = "k8s.v1.cni.cncf.io/resourceName"
	NodeSelectorKey    = "k8s.v1.cni.cncf.io/nodeSelector"
	ExtProjectIDKey    = "nokia.com/extProjectID"
	ExtNetworkIDKey    = "nokia.com/extNetworkID"
	SriovOverlaysKey   = "nokia.com/sriov-vf-vlan-trunk-overlays"
	NetworkTopologyKey = "nokia.com/network-topology"
	NetworkStatusKey   = "nokia.com/network-status"
)

type Nic struct {
	Name       string `json:"name"`
	MacAddress string `json:"mac-address"`
}

// NIC in JSON format
type JsonNic map[string]interface{}
type NicMap map[string]JsonNic

type NodeTopology struct {
	Bonds      map[string]NicMap
	SriovPools map[string]NicMap
}

type NetConf struct {
	types.NetConf
	Master    string `json:"master,omitempty"`
	Vlan      int    `json:"vlan,omitempty"`
	VlanTrunk string `json:"vlan_trunk,omitempty"`
}

type NadAction int

const (
	//Create ... NAD created ==> create host interface
	Create NadAction = 1
	//Delete ... NAD deleted ==> delete host interface
	Delete NadAction = 2
	//Attach ... NAD created ==> open vlan on switch
	CreateAttach NadAction = 3
	//Detach ... NAD deleted ==> close vlan on switch
	DeleteDetach NadAction = 4
	//AttachDetach ... NAD updated ==> nodeSelector changed
	UpdateAttachDetach NadAction = 5
	//UpdateAttach ... NAD updated ==> becomes in scope (optional content)
	UpdateAttach NadAction = 6
	//UpdateDetach ... NAD updated ==> becomes out of scope (optional content)
	UpdateDetach NadAction = 7
	//NodeAttach ... open vlan on switch
	NodeAttach NadAction = 8
	//Detach ... close vlan on switch
	NodeDetach NadAction = 9
	//AttachDetach ... nodes using vlan changed
	NodeAttachDetach NadAction = 10
)

func GetVlanIds(vlanTrunk string) ([]int, error) {
	result := []int{}
	err := fmt.Errorf("Trunk format is invalid, it should follow this pattern 50,51,700-710")
	if !regexp.MustCompile(`^[0-9\,\-]*$`).MatchString(vlanTrunk) {
		return result, err
	}
	m := strings.Split(vlanTrunk, ",")
	for _, v := range m {
		if strings.Contains(v, "-") {
			n := strings.Split(v, "-")
			if len(n) != 2 {
				return result, err
			}
			min, _ := strconv.Atoi(n[0])
			max, _ := strconv.Atoi(n[1])
			if min == 0 || min > max {
				return result, err
			}
			count := max - min + 1
			for i := 0; i < count; i++ {
				result = append(result, min+i)
			}
		} else {
			vi, _ := strconv.Atoi(v)
			result = append(result, vi)
		}
	}
	return result, nil
}

func GetNetConf(nad *netattachdef.NetworkAttachmentDefinition) (NetConf, error) {
	// Read NAD Config
	var netConf NetConf
	var config map[string]interface{}
	if err := json.Unmarshal([]byte(nad.Spec.Config), &config); err != nil {
		return netConf, fmt.Errorf("read NAD config failed: %s", err.Error())
	}

	// Check if CNI config has plugin
	if p, ok := config["plugins"]; ok {
		plugins := p.([]interface{})
		for _, v := range plugins {
			plugin := v.(map[string]interface{})
			if plugin["type"] == "ipvlan" || plugin["type"] == "sriov" {
				confBytes, _ := json.Marshal(v)
				json.Unmarshal(confBytes, &netConf)
				break
			}
		}
	} else {
		json.Unmarshal([]byte(nad.Spec.Config), &netConf)
	}
	return netConf, nil
}

func ShouldTriggerTopoAction(nad *netattachdef.NetworkAttachmentDefinition) (NetConf, bool, error) {
	// Get NAD Config
	netConf, err := GetNetConf(nad)
	if err != nil {
		return netConf, false, err
	}

	if netConf.Type != "ipvlan" && netConf.Type != "sriov" {
		return netConf, false, nil
	}

	// Check nodeSelector
	annotationsMap := nad.GetAnnotations()
	ns, ok := annotationsMap[NodeSelectorKey]
	if !ok || len(ns) == 0 {
		return netConf, false, nil
	}
	// Check NAD type
	if netConf.Type == "ipvlan" {
		// Check extProjectID
		project, ok := annotationsMap[ExtProjectIDKey]
		if !ok || len(project) == 0 {
			return netConf, false, nil
		}
		// Check extNetworkID
		network, ok := annotationsMap[ExtNetworkIDKey]
		if !ok || len(network) == 0 {
			return netConf, false, nil
		}
		return netConf, true, nil
	}
	if netConf.Type == "sriov" {
		resourceName, ok := annotationsMap[SriovResourceKey]
		if !ok || len(resourceName) == 0 {
			return netConf, false, fmt.Errorf("SRIOV NAD requires resource name")
		}
		if len(netConf.VlanTrunk) > 0 {
			vlanIds, err := GetVlanIds(netConf.VlanTrunk)
			if err != nil {
				return netConf, false, fmt.Errorf("Invalid vlan_trunk in CNI: %s", err.Error())
			}
			// Check Overlays
			jsonOverlays, ok := annotationsMap[SriovOverlaysKey]
			if !ok || len(jsonOverlays) == 0 {
				return netConf, false, fmt.Errorf("Missing %s in annotations", SriovOverlaysKey)
			}
			var overlays []map[string]string
			err = json.Unmarshal([]byte(jsonOverlays), &overlays)
			if err != nil {
				return netConf, false, err
			}
			var vlanRanges []string
			for _, overlay := range overlays {
				_, ok1 := overlay["extProjectID"]
				_, ok2 := overlay["extNetworkID"]
				vlanRange, ok3 := overlay["vlanRange"]
				if !ok1 || !ok2 || !ok3 {
					return netConf, false, fmt.Errorf("Invalid overlay value in %s", overlay)
				}
				_, err = GetVlanIds(vlanRange)
				if err != nil {
					return netConf, false, fmt.Errorf("Invalid vlan range %s in %s: %s", vlanRange, overlay, err.Error())
				}
				vlanRanges = append(vlanRanges, vlanRange)
			}
			vlanTrunk := strings.Join(vlanRanges, ",")
			overlayVlanIds, _ := GetVlanIds(vlanTrunk)
			sort.Ints(overlayVlanIds)
			sort.Ints(vlanIds)
			if !reflect.DeepEqual(vlanIds, overlayVlanIds) {
				return netConf, false, fmt.Errorf("Different vlan ranges found in CNI and annotations")
			}
			return netConf, true, nil
		} else {
			if netConf.Vlan >= 0 && netConf.Vlan <= 4095 {
				// Check extProjectID
				project, ok := annotationsMap[ExtProjectIDKey]
				if !ok || len(project) == 0 {
					return netConf, false, nil
				}
				// Check extNetworkID
				network, ok := annotationsMap[ExtNetworkIDKey]
				if !ok || len(network) == 0 {
					return netConf, false, nil
				}
				return netConf, true, nil
                        } else {
                                return netConf, false, fmt.Errorf("vlan value is out of bound, valid range (0..4095) ")
                        }
		}
	}
	return netConf, false, nil
}

func ShouldTriggerTopoUpdate(oldNad, newNad *netattachdef.NetworkAttachmentDefinition) (NadAction, error) {
	// Check NAD for action
	oldNetConf, trigger1, _ := ShouldTriggerTopoAction(oldNad)
	newNetConf, trigger2, _ := ShouldTriggerTopoAction(newNad)

	if !trigger1 && !trigger2 {
		return 0, nil
	}
	if !trigger1 && trigger2 {
		return UpdateAttach, nil
	}
	if trigger1 && !trigger2 {
		return 0, fmt.Errorf("NAD change from FSS eligible to not eligble is not allowed")
	}
	// Handle network change
	if oldNetConf.Type != newNetConf.Type {
		return 0, fmt.Errorf("NAD type change is not allowed")
	}
	if oldNetConf.Vlan > 0 && oldNetConf.Vlan != newNetConf.Vlan {
		return 0, fmt.Errorf("NAD vlan change is not allowed")
	}
	if oldNetConf.Vlan == 0 && oldNetConf.VlanTrunk != newNetConf.VlanTrunk {
		return 0, fmt.Errorf("SRIOV NAD vlan_trunk change is not allowed")
	}
	anno1 := oldNad.GetAnnotations()
	anno2 := newNad.GetAnnotations()
	if newNetConf.Type == "sriov" && newNetConf.Vlan == 0 {
		sriovOverlays1, _ := anno1[SriovOverlaysKey]
		sriovOverlays2, _ := anno2[SriovOverlaysKey]
		if sriovOverlays1 != sriovOverlays2 {
			return 0, fmt.Errorf("NAD SRIOV overlays change is not allowed")
		}
	} else {
		proj1, _ := anno1[ExtProjectIDKey]
		net1, _ := anno1[ExtNetworkIDKey]
		proj2, _ := anno2[ExtProjectIDKey]
		net2, _ := anno2[ExtNetworkIDKey]
		if proj1 != proj2 {
			return 0, fmt.Errorf("NAD project change is not allowed")
		}
		if net1 != net2 {
			return 0, fmt.Errorf("NAD network change is not allowed")
		}
	}
	ns1, _ := anno1[NodeSelectorKey]
	ns2, _ := anno2[NodeSelectorKey]
	if ns1 == ns2 {
		return 0, nil
	}
	return UpdateAttachDetach, nil
}
