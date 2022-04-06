// Copyright (c) 2018 Intel Corporation
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

package webhook

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/containernetworking/cni/libcni"
	"github.com/golang/glog"
	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/pkg/errors"
	"gopkg.in/intel/multus-cni.v3/pkg/types"
	"k8s.io/api/admission/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type NetConf struct {
	types.NetConf
	Master string `json:"master,omitempty"`
	Vlan   int    `json:"vlan,omitempty"`
}

type jsonPatchOperation struct {
	Operation string      `json:"op"`
	Path      string      `json:"path"`
	Value     interface{} `json:"value,omitempty"`
}

const (
	nodeSelectorKey        = "k8s.v1.cni.cncf.io/nodeSelector"
	networksAnnotationKey  = "k8s.v1.cni.cncf.io/networks"
	networkResourceNameKey = "k8s.v1.cni.cncf.io/resourceName"
	namespaceConstraint    = "_local"
)

var (
	clientset kubernetes.Interface
)

// validateCNIConfig verifies following fields
// conf: 'type'
// conflist: 'plugins' and 'type'
func validateCNIConfig(config []byte) error {
	var c map[string]interface{}
	if err := json.Unmarshal(config, &c); err != nil {
		return err
	}

	// Identify target is single CNI config or plugins
	if p, ok := c["plugins"]; ok {
		// CNI conflist
		// check 'type' field for each plugin in 'plugins'
		plugins := p.([]interface{})
		for _, v := range plugins {
			plugin := v.(map[string]interface{})
			if _, ok := plugin["type"]; !ok {
				return fmt.Errorf("missing 'type' in plugins")
			}
		}
	} else {
		// single CNI config
		if _, ok := c["type"]; !ok {
			return fmt.Errorf("missing 'type' in cni config")
		}
	}
	return nil
}

//getInfraVlanData returns vlan ranges used by cloud infra-structure
func getInfraVlanData() ([]int, error) {
	var infraVlans []int

	fs := os.Getenv("SRIOV_ON_NIC_1_ENABLED")
	if fs == "" {
		return infraVlans, nil
	}
	fv, err := strconv.ParseBool(fs)
	if err != nil {
		return infraVlans, err
	}
	if fv {
		ds := os.Getenv("INFRA_VLAN_RANGE")
		if ds == "" {
			return infraVlans, nil
		}
		dv := strings.Split(ds, " ")
		infraVlans = make([]int, len(dv))
		for i := range dv {
			infraVlans[i], _ = strconv.Atoi(dv[i])
		}
	}

	return infraVlans, nil
}

// validateCNIConfigSriov verifies following fields
// conf: 'vlan' and 'vlanTrunkString'
func validateCNIConfigSriov(config []byte) error {
	var c map[string]interface{}
	if err := json.Unmarshal(config, &c); err != nil {
		return err
	}

	if cniType, ok := c["type"]; ok {
		if cniType == "sriov" {
			infraVlans, err := getInfraVlanData()
			if err != nil || len(infraVlans) == 0 {
				return nil
			}
			vlan, vlanExists := c["vlan"]
			vlanTrunk, vlanTrunkExists := c["vlan_trunk"]
			if vlanExists && vlanTrunkExists {
				return fmt.Errorf("both vlan and vlan_trunk fields are defined")
			}
			if !vlanExists && !vlanTrunkExists {
				return fmt.Errorf("either vlan or vlan_trunk field should be defined")
			}
			if vlanExists {
				vlanString := fmt.Sprintf("%v", vlan)
				vlanId, err1 := strconv.Atoi(vlanString)
				if err1 != nil {
					return fmt.Errorf("vlan field format error")
				}
				for i := 0; i < len(infraVlans); i++ {
					if infraVlans[i] == vlanId {
						return fmt.Errorf("infrastructure vlan id %d shall not be used in vlan field", infraVlans[i])
					}
				}
			}
			if vlanTrunkExists {
				vlanTrunkString := fmt.Sprintf("%v", vlanTrunk)
				trunkingRanges := strings.Split(vlanTrunkString, ",")
				for _, r := range trunkingRanges {
					values := strings.Split(r, "-")
					v1, err1 := strconv.Atoi(values[0])
					v2, err2 := strconv.Atoi(values[len(values)-1])

					if err1 != nil || err2 != nil {
						return fmt.Errorf("vlan_trunk field format error")
					}

					if v1 > v2 || v1 < 1 || v2 > 4095 {
						return fmt.Errorf("vlan_trunk field range error")
					}

					for i := 0; i < len(infraVlans); i++ {
						if infraVlans[i] >= v1 && infraVlans[i] <= v2 {
							return fmt.Errorf("infrastructure vlan id %d shall not be used in vlan_trunk field", infraVlans[i])
						}
					}
				}
			}
			qos, qosExists := c["vlanQoS"]
			if qosExists {
				qosString := fmt.Sprintf("%v", qos)
				qosId, err1 := strconv.Atoi(qosString)
				if err1 != nil {
					return fmt.Errorf("qos field format error")
				}
				if qosId != 0 {
					return fmt.Errorf("qos %v is defined while only default qos (0) is allowed", qosId)
				}
			}
		}
	}
	return nil
}

// preprocessCNIConfig process CNI config bytes as following (that multus does too)
// - if 'name' is missing, 'name' is filled
func preprocessCNIConfig(name string, config []byte) ([]byte, error) {
	var c map[string]interface{}
	if err := json.Unmarshal(config, &c); err != nil {
		if n, ok := c["name"]; !ok || n == "" {
			c["name"] = name
		}
	}
	configBytes, err := json.Marshal(c)
	return configBytes, err
}

// isJSON detects if a string is in JSON format
func isJSON(s string) bool {
	var js map[string]interface{}
	return json.Unmarshal([]byte(s), &js) == nil
}

func validateNetworkAttachmentDefinition(operation v1beta1.Operation, netAttachDef netv1.NetworkAttachmentDefinition, oldNad netv1.NetworkAttachmentDefinition) (bool, bool, error) {
	nameRegex := `^[a-z-1-9]([-a-z0-9]*[a-z0-9])?$`
	isNameCorrect, err := regexp.MatchString(nameRegex, netAttachDef.GetName())
	if !isNameCorrect {
		err := errors.New("net-attach-def name is invalid")
		glog.Info(err)
		return false, false, err
	}
	if err != nil {
		err := errors.New("error validating name")
		glog.Error(err)
		return false, false, err
	}

	glog.Infof("validating network config spec: %s", netAttachDef.Spec.Config)

	var confBytes []byte
        var mutationRequired bool = false
	if netAttachDef.Spec.Config != "" {
		// try to unmarshal config into NetworkConfig or NetworkConfigList
		//  using actual code from libcni - if succesful, it means that the config
		//  will be accepted by CNI itself as well
		if !isJSON(netAttachDef.Spec.Config) {
			err := errors.New("configuration string is not in JSON format")
			glog.Info(err)
			return false, false, err
		}

		confBytes, err = preprocessCNIConfig(netAttachDef.GetName(), []byte(netAttachDef.Spec.Config))
		if err != nil {
			err := errors.New("invalid json")
			return false, false, err
		}
		if err := validateCNIConfig(confBytes); err != nil {
			err := errors.New("invalid config")
			return false, false, err
		}
                // additional validation on sriov type
                if err := validateCNIConfigSriov(confBytes); err != nil {
                        err := errors.New(err.Error())
                        return false, false, err
                }
                _, err = libcni.ConfListFromBytes(confBytes)
                if err != nil {
                        glog.Infof("spec is not a valid network config list: %s - trying to parse into standalone config", err)
                        _, err = libcni.ConfFromBytes(confBytes)
                        if err != nil {
                                glog.Infof("spec is not a valid network config: %s", confBytes)
                                err := errors.New("invalid config")
                                return false, false, err
                        }
                }
                // additional validation on ipvlan type
                mutate, err := validateCNIIpvlanConfig(operation, netAttachDef, oldNad)
                if err != nil {
                        err := errors.New(err.Error())
                        return false, false, err
                }
                mutationRequired = mutate
	} else {
		glog.Infof("Allowing empty spec.config")
	}

	glog.Infof("AdmissionReview request allowed: Network Attachment Definition '%s' is valid", confBytes)
	return true, mutationRequired, nil
}

// validateCNIIpvlanConfig verifies following fields
// conf: 'master' and 'vlan'
// annotatoin: 'nodeSelector'
// also check if mutation is needed
// return netConfig, mutationRequired, and err for ipvlan validation error
func shouldTriggerAction(netAttachDef netv1.NetworkAttachmentDefinition) (NetConf, bool, error) {
	// Read NAD Config
	var netConf NetConf
	json.Unmarshal([]byte(netAttachDef.Spec.Config), &netConf)
	// Check NAD type, skip check if it is not ipvlan
	if netConf.Type != "ipvlan" {
		return netConf, false, nil
	}
	if netConf.Vlan < 1 || netConf.Vlan > 4095 {
		return netConf, false, fmt.Errorf("ipvlan vlan value out of bound. Valid range 1..4095")
	}
        if !strings.HasPrefix(netConf.Master, "tenant-bond") && !strings.HasPrefix(netConf.Master, "provider-bond"){
                return netConf, false, fmt.Errorf("ipvlan only support master with tenant-bond and provider-bond")
        }
	// Check nodeSelector
	annotationsMap := netAttachDef.GetAnnotations()
	ns, ok := annotationsMap[nodeSelectorKey]
	if !ok || len(ns) == 0 {
		return netConf, false, fmt.Errorf("NAD with ipvlan, but nodeSelector is not present")
	}

        //check if mutation has already been done
        if strings.HasPrefix(netConf.Master, "tenant-bond.") || strings.HasPrefix(netConf.Master, "provider-bond.") {
                m := strings.Split(netConf.Master, ".")
                v, err := strconv.Atoi(m[1])
                if err != nil {
                        return netConf, false, fmt.Errorf("IPVLAN master field is incorrect")
                }
                if v != netConf.Vlan {
                        return netConf, false, fmt.Errorf("IPVLAN master field is incorrect")
                }
                //mutation is already done, no need to mutate
                return netConf, false, nil
        }

	return netConf, true, nil
}

func validateCNIIpvlanConfig(operation v1beta1.Operation, netAttachDef netv1.NetworkAttachmentDefinition, oldNad netv1.NetworkAttachmentDefinition) (bool, error) {
	netConf, mutationRequired, err := shouldTriggerAction(netAttachDef)
        if err != nil {
                return false, fmt.Errorf("Failed to validate IPVLAN config: %v", err)
        }

        //NAD update for ipvlan with master and vlan field change is not allowed
	if netConf.Type == "ipvlan" && operation == "UPDATE" {
	        oldConf, _, _ := shouldTriggerAction(oldNad)
	        if oldConf.Master != netConf.Master && oldConf.Master != netConf.Master+"."+strconv.Itoa(netConf.Vlan) {
		        glog.Error("master and vlan field shall not change, you should delete and re-create")
	                return false, fmt.Errorf("IPVLAN master and vlan field change is not allowed")
	        }
	}

	return mutationRequired, nil
}

func mutateNetworkAttachmentDefinition(netAttachDef netv1.NetworkAttachmentDefinition, patch []jsonPatchOperation) []jsonPatchOperation {
	// Read NAD Config
	var netConf NetConf
	json.Unmarshal([]byte(netAttachDef.Spec.Config), &netConf)
	vlanIfName := netConf.Master + "." + strconv.Itoa(netConf.Vlan)
	var c map[string]interface{}
	json.Unmarshal([]byte(netAttachDef.Spec.Config), &c)
	c["master"] = vlanIfName
	configBytes, _ := json.Marshal(c)
	netAttachDef.Spec.Config = string(configBytes)
        glog.V(5).Info("Mutate: Network Attachment Definition '%s'", netAttachDef.Spec.Config)

	patch = append(patch, jsonPatchOperation{
		Operation: "replace",
		Path:      "/spec/config",
		Value:     netAttachDef.Spec.Config,
	})
	return patch
}

func prepareAdmissionReviewResponse(allowed bool, message string, ar *v1beta1.AdmissionReview) error {
	if ar.Request != nil {
		ar.Response = &v1beta1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: allowed,
		}
		if message != "" {
			ar.Response.Result = &metav1.Status{
				Message: message,
			}
		}
		return nil
	}
	return errors.New("received empty AdmissionReview request")
}

func readAdmissionReview(req *http.Request) (*v1beta1.AdmissionReview, int, error) {
	var body []byte

	if req.Body != nil {
		if data, err := ioutil.ReadAll(req.Body); err == nil {
			body = data
		}
	}

	if len(body) == 0 {
		err := errors.New("Error reading HTTP request: empty body")
		glog.Error(err)
		return nil, http.StatusBadRequest, err
	}

	/* validate HTTP request headers */
	contentType := req.Header.Get("Content-Type")
	if contentType != "application/json" {
		err := errors.Errorf("Invalid Content-Type='%s', expected 'application/json'", contentType)
		glog.Error(err)
		return nil, http.StatusUnsupportedMediaType, err
	}

	/* read AdmissionReview from the request body */
	ar, err := deserializeAdmissionReview(body)
	if err != nil {
		err := errors.Wrap(err, "error deserializing AdmissionReview")
		glog.Error(err)
		return nil, http.StatusBadRequest, err
	}

	return ar, http.StatusOK, nil
}

func deserializeAdmissionReview(body []byte) (*v1beta1.AdmissionReview, error) {
	ar := &v1beta1.AdmissionReview{}
	runtimeScheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(runtimeScheme)
	deserializer := codecs.UniversalDeserializer()
	_, _, err := deserializer.Decode(body, nil, ar)

	/* Decode() won't return an error if the data wasn't actual AdmissionReview */
	if err == nil && ar.TypeMeta.Kind != "AdmissionReview" {
		err = errors.New("received object is not an AdmissionReview")
	}

	return ar, err
}

func analyzeIsolationAnnotation(ar *v1beta1.AdmissionReview) (bool, error) {

	var metadata *metav1.ObjectMeta
	var pod v1.Pod

	req := ar.Request

	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		glog.Errorf("Could not unmarshal raw object: %v", err)
		return false, err
	}

	metadata = &pod.ObjectMeta
	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	if len(annotations[networksAnnotationKey]) > 0 {

		glog.Infof("Analyzing %s annotation: %s", networksAnnotationKey, annotations[networksAnnotationKey])

		networks, err := parsePodNetworkAnnotation(annotations[networksAnnotationKey], namespaceConstraint)
		if err != nil {
			glog.Errorf("Error during parsePodNetworkAnnotation: %v", err)
			return false, err
		}

		for _, item := range networks {
			fmt.Printf("name: %v", item.Namespace)
			if item.Namespace != namespaceConstraint {
				annotationerrorstring := fmt.Sprintf("%s annotations must not refer to namespaced values (must use local namespace, i.e. must not contain a /), rejected: %s (namespace: %s)", networksAnnotationKey, annotations[networksAnnotationKey], item.Namespace)
				annotationerror := errors.New(annotationerrorstring)
				return false, annotationerror
			}
		}

		glog.Infof("Allowed value: %s", annotations[networksAnnotationKey])

	}

	return true, nil

}

func parsePodNetworkAnnotation(podNetworks, defaultNamespace string) ([]*types.NetworkSelectionElement, error) {
	var networks []*types.NetworkSelectionElement

	// logging.Debugf("parsePodNetworkAnnotation: %s, %s", podNetworks, defaultNamespace)
	if podNetworks == "" {
		return nil, fmt.Errorf("parsePodNetworkAnnotation: pod annotation not having \"network\" as key, refer Multus README.md for the usage guide")
	}

	if strings.IndexAny(podNetworks, "[{\"") >= 0 {
		if err := json.Unmarshal([]byte(podNetworks), &networks); err != nil {
			return nil, fmt.Errorf("parsePodNetworkAnnotation: failed to parse pod Network Attachment Selection Annotation JSON format: %v", err)
		}
	} else {
		// Comma-delimited list of network attachment object names
		for _, item := range strings.Split(podNetworks, ",") {
			// Remove leading and trailing whitespace.
			item = strings.TrimSpace(item)

			// Parse network name (i.e. <namespace>/<network name>@<ifname>)
			netNsName, networkName, netIfName, err := parsePodNetworkObjectName(item)
			if err != nil {
				return nil, fmt.Errorf("parsePodNetworkAnnotation: %v", err)
			}

			networks = append(networks, &types.NetworkSelectionElement{
				Name:             networkName,
				Namespace:        netNsName,
				InterfaceRequest: netIfName,
			})
		}
	}

	for _, net := range networks {
		if net.Namespace == "" {
			net.Namespace = defaultNamespace
		}
	}

	return networks, nil
}

func parsePodNetworkObjectName(podnetwork string) (string, string, string, error) {
	var netNsName string
	var netIfName string
	var networkName string

	// logging.Debugf("parsePodNetworkObjectName: %s", podnetwork)
	slashItems := strings.Split(podnetwork, "/")
	if len(slashItems) == 2 {
		netNsName = strings.TrimSpace(slashItems[0])
		networkName = slashItems[1]
	} else if len(slashItems) == 1 {
		networkName = slashItems[0]
	} else {
		return "", "", "", fmt.Errorf("Invalid network object (failed at '/')")
	}

	atItems := strings.Split(networkName, "@")
	networkName = strings.TrimSpace(atItems[0])
	if len(atItems) == 2 {
		netIfName = strings.TrimSpace(atItems[1])
	} else if len(atItems) != 1 {
		return "", "", "", fmt.Errorf("Invalid network object (failed at '@')")
	}

	// Check and see if each item matches the specification for valid attachment name.
	// "Valid attachment names must be comprised of units of the DNS-1123 label format"
	// [a-z0-9]([-a-z0-9]*[a-z0-9])?
	// And we allow at (@), and forward slash (/) (units separated by commas)
	// It must start and end alphanumerically.
	allItems := []string{netNsName, networkName, netIfName}
	for i := range allItems {
		matched, _ := regexp.MatchString("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", allItems[i])
		if !matched && len([]rune(allItems[i])) > 0 {
			return "", "", "", fmt.Errorf(fmt.Sprintf("Failed to parse: one or more items did not match comma-delimited format (must consist of lower case alphanumeric characters). Must start and end with an alphanumeric character), mismatch @ '%v'", allItems[i]))
		}
	}

	// logging.Debugf("parsePodNetworkObjectName: parsed: %s, %s, %s", netNsName, networkName, netIfName)
	return netNsName, networkName, netIfName, nil
}

func deserializeNetworkAttachmentDefinition(ar *v1beta1.AdmissionReview) (netv1.NetworkAttachmentDefinition, netv1.NetworkAttachmentDefinition, error) {
	/* unmarshal NetworkAttachmentDefinition from AdmissionReview request */
	netAttachDef := netv1.NetworkAttachmentDefinition{}
	oldNad := netv1.NetworkAttachmentDefinition{}
	err := json.Unmarshal(ar.Request.Object.Raw, &netAttachDef)
	if err == nil && ar.Request.Operation == "UPDATE" {
		err = json.Unmarshal(ar.Request.OldObject.Raw, &oldNad)
	}
	return netAttachDef, oldNad, err
}

func handleValidationError(w http.ResponseWriter, ar *v1beta1.AdmissionReview, orgErr error) {
	err := prepareAdmissionReviewResponse(false, orgErr.Error(), ar)
	if err != nil {
		err := errors.Wrap(err, "error preparing AdmissionResponse")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeResponse(w, ar)
}

func writeResponse(w http.ResponseWriter, ar *v1beta1.AdmissionReview) {
	// glog.Infof("sending response to the Kubernetes API server")
	resp, _ := json.Marshal(ar)
	w.Write(resp)
}

// IsolateHandler Handles namespace isolation validation.
func IsolateHandler(w http.ResponseWriter, req *http.Request) {

	var allowed bool

	ar, httpStatus, err := readAdmissionReview(req)
	if err != nil {
		http.Error(w, err.Error(), httpStatus)
		return
	}

	allowed, err = analyzeIsolationAnnotation(ar)
	if err != nil {
		handleValidationError(w, ar, err)
		return
	}

	err = prepareAdmissionReviewResponse(allowed, "", ar)
	if err != nil {
		glog.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeResponse(w, ar)
}

// ValidateHandler handles net-attach-def validation requests
func ValidateHandler(w http.ResponseWriter, req *http.Request) {
	/* read AdmissionReview from the HTTP request */
	ar, httpStatus, err := readAdmissionReview(req)
	if err != nil {
		http.Error(w, err.Error(), httpStatus)
		return
	}

	netAttachDef, oldNad, err := deserializeNetworkAttachmentDefinition(ar)
	if err != nil {
		handleValidationError(w, ar, err)
		return
	}

	/* perform actual object validation */
	allowed, mutationRequired, err := validateNetworkAttachmentDefinition(ar.Request.Operation, netAttachDef, oldNad)
	if err != nil {
		handleValidationError(w, ar, err)
		return
	}

	/* perpare response and send it back to the API server */
	err = prepareAdmissionReviewResponse(allowed, "", ar)
	if err != nil {
		glog.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if allowed && mutationRequired {
		var patch []jsonPatchOperation
		patch = mutateNetworkAttachmentDefinition(netAttachDef, patch)
		ar.Response.Patch, _ = json.Marshal(patch)
		ar.Response.PatchType = func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}()

	}
	writeResponse(w, ar)
}

// SetupInClusterClient sets up api configuration
func SetupInClusterClient() {
	/* setup Kubernetes API client */
	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatal(err)
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatal(err)
	}
}
