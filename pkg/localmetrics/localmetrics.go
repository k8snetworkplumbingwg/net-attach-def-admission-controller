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
	NetDefCounter = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "network_attachment_definitions_count",
			Help: "Metric to count network attachment definitions.",
		})

	MultusPodCounter = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "multus_pod_count",
			Help: "Metric to get total pods using multus.",
		})
)

// UpdateNetDefMetrics
func UpdateNetDefMetrics(value float64) {
	NetDefCounter.Add(value)
}

//UpdateMultusPodMetrics
func UpdateMultusPodMetrics(value float64) {
	MultusPodCounter.Add(value)
}

//SetMultusPodMetrics
func SetMultusPodMetrics(value float64) {
	MultusPodCounter.Set(value)
}
