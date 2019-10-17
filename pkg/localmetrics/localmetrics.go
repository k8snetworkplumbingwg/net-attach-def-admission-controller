// Copyright 2019 RedHat
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

package localmetrics

import (
	"github.com/prometheus/client_golang/prometheus"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("netdefattachment")
var (
	multusPodEnabledCount = 0.0
	//NetDefCounter .. increments count for every valid netdef crd
	NetDefCounter = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "network_attachment_definitions_count",
			Help: "Metric to count network attachment definitions.",
		})
	//MultusPodCounter ...  Total no of multus pods in the cluster
	MultusPodCounter = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "multus_pod_count",
			Help: "Metric to get total pods using multus.",
		})
	//MultusEnabledPodsUp  ... check if any pods with multus config enabled
	MultusEnabledPodsUp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "multus_enabled_instance_up",
			Help: "Metric to identify clusters with multus enabled instances.",
		})
)

// UpdateNetDefMetrics ...
func UpdateNetDefMetrics(value float64) {
	NetDefCounter.Add(value)
}

//UpdateMultusPodMetrics ...
func UpdateMultusPodMetrics(value float64) {
	MultusPodCounter.Add(value)
	multusPodEnabledCount += value
	if multusPodEnabledCount > 0.0 {
		SetMultusEnabledPodUp(1.0)
	} else {
		SetMultusEnabledPodUp(0.0)
	}
}

//SetMultusEnabledPodUp ...
func SetMultusEnabledPodUp(value float64) {
	MultusEnabledPodsUp.Set(value)
}
