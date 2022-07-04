package datatypes

import (
	"gopkg.in/intel/multus-cni.v3/pkg/types"
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
