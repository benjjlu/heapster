// Copyright 2015 Google Inc. All Rights Reserved.
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

package summary

import (
	"fmt"
	"net/url"
	"time"

	. "k8s.io/heapster/metrics/core"
	"k8s.io/heapster/metrics/sources/kubelet"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/labels"
	kube_client "k8s.io/client-go/kubernetes"
	v1listers "k8s.io/client-go/listers/core/v1"
	kube_api "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/heapster/metrics/util"
	"k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/stats"
)

var (
	summaryRequestLatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace: "heapster",
			Subsystem: "kubelet_summary",
			Name:      "request_duration_microseconds",
			Help:      "The Kubelet summary request latencies in microseconds.",
		},
		[]string{"node"},
	)
)

// Prefix used for the LabelResourceID for volume metrics.
const VolumeResourcePrefix = "Volume:"

func init() {
	prometheus.MustRegister(summaryRequestLatency)
}

type NodeInfo struct {
	kubelet.Host
	NodeName       string
	HostName       string
	HostID         string
	KubeletVersion string
}

// Kubelet-provided metrics for pod and system container.
type summaryMetricsSource struct {
	node          NodeInfo
	kubeletClient *kubelet.KubeletClient
}

func NewSummaryMetricsSource(node NodeInfo, client *kubelet.KubeletClient) MetricsSource {
	return &summaryMetricsSource{
		node:          node,
		kubeletClient: client,
	}
}

func (this *summaryMetricsSource) Name() string {
	return this.String()
}

func (this *summaryMetricsSource) String() string {
	return fmt.Sprintf("kubelet_summary:%s:%d", this.node.IP, this.node.Port)
}

func (this *summaryMetricsSource) ScrapeMetrics(start, end time.Time) *DataBatch {
	result := &DataBatch{
		Timestamp:  time.Now(),
		MetricSets: map[string]*MetricSet{},
	}

	summary, err := func() (*stats.Summary, error) {
		startTime := time.Now()
		defer summaryRequestLatency.WithLabelValues(this.node.HostName).Observe(float64(time.Since(startTime)))
		return this.kubeletClient.GetSummary(this.node.Host)
	}()

	if err != nil {
		glog.Errorf("error while getting metrics summary from Kubelet %s(%s:%d): %v", this.node.NodeName, this.node.IP, this.node.Port, err)
		return result
	}

	result.MetricSets = this.decodeSummary(summary)

	return result
}

const (
	RootFsKey = "/"
	LogsKey   = "logs"
)

// For backwards compatibility, map summary system names into original names.
// TODO: Migrate to the new system names and remove this.
var systemNameMap = map[string]string{
	stats.SystemContainerRuntime: "docker-daemon",
	stats.SystemContainerMisc:    "system",
}

// decodeSummary translates the kubelet stats.Summary API into the flattened heapster MetricSet API.
func (this *summaryMetricsSource) decodeSummary(summary *stats.Summary) map[string]*MetricSet {
	glog.V(9).Infof("Begin summary decode")
	result := map[string]*MetricSet{}

	labels := map[string]string{
		LabelNodename.Key: this.node.NodeName,
		LabelHostname.Key: this.node.HostName,
		LabelHostID.Key:   this.node.HostID,
	}

	this.decodeNodeStats(result, labels, &summary.Node)
	for _, pod := range summary.Pods {
		this.decodePodStats(result, labels, &pod)
	}

	glog.V(9).Infof("End summary decode")
	return result
}

// Convenience method for labels deep copy.
func (this *summaryMetricsSource) cloneLabels(labels map[string]string) map[string]string {
	clone := make(map[string]string, len(labels))
	for k, v := range labels {
		clone[k] = v
	}
	return clone
}

func (this *summaryMetricsSource) decodeNodeStats(metrics map[string]*MetricSet, labels map[string]string, node *stats.NodeStats) {
	glog.V(9).Infof("Decoding node stats for node %s...", node.NodeName)
	nodeMetrics := &MetricSet{
		Labels:         this.cloneLabels(labels),
		MetricValues:   map[string]MetricValue{},
		LabeledMetrics: []LabeledMetric{},
		CreateTime:     node.StartTime.Time,
		ScrapeTime:     this.getScrapeTime(node.CPU, node.Memory, node.Network),
	}
	nodeMetrics.Labels[LabelMetricSetType.Key] = MetricSetTypeNode

	this.decodeUptime(nodeMetrics, node.StartTime.Time)
	this.decodeCPUStats(nodeMetrics, node.CPU)
	this.decodeMemoryStats(nodeMetrics, node.Memory)
	this.decodeNetworkStats(nodeMetrics, node.Network)
	this.decodeFsStats(nodeMetrics, RootFsKey, node.Fs)
	metrics[NodeKey(node.NodeName)] = nodeMetrics

	for _, container := range node.SystemContainers {
		key := NodeContainerKey(node.NodeName, this.getSystemContainerName(&container))
		containerMetrics := this.decodeContainerStats(labels, &container, true)
		containerMetrics.Labels[LabelMetricSetType.Key] = MetricSetTypeSystemContainer
		metrics[key] = containerMetrics
	}
}

func (this *summaryMetricsSource) decodePodStats(metrics map[string]*MetricSet, nodeLabels map[string]string, pod *stats.PodStats) {
	glog.V(9).Infof("Decoding pod stats for pod %s/%s (%s)...", pod.PodRef.Namespace, pod.PodRef.Name, pod.PodRef.UID)
	podMetrics := &MetricSet{
		Labels:         this.cloneLabels(nodeLabels),
		MetricValues:   map[string]MetricValue{},
		LabeledMetrics: []LabeledMetric{},
		CreateTime:     pod.StartTime.Time,
		ScrapeTime:     this.getScrapeTime(nil, nil, pod.Network),
	}
	ref := pod.PodRef
	podMetrics.Labels[LabelMetricSetType.Key] = MetricSetTypePod
	podMetrics.Labels[LabelPodId.Key] = ref.UID
	podMetrics.Labels[LabelPodName.Key] = ref.Name
	podMetrics.Labels[LabelNamespaceName.Key] = ref.Namespace

	this.decodeUptime(podMetrics, pod.StartTime.Time)
	this.decodeNetworkStats(podMetrics, pod.Network)
	for _, vol := range pod.VolumeStats {
		this.decodeFsStats(podMetrics, VolumeResourcePrefix+vol.Name, &vol.FsStats)
	}
	metrics[PodKey(ref.Namespace, ref.Name)] = podMetrics

	for _, container := range pod.Containers {
		key := PodContainerKey(ref.Namespace, ref.Name, container.Name)
		metrics[key] = this.decodeContainerStats(podMetrics.Labels, &container, false)
	}
}

func (this *summaryMetricsSource) decodeContainerStats(podLabels map[string]string, container *stats.ContainerStats, isSystemContainer bool) *MetricSet {
	glog.V(9).Infof("Decoding container stats stats for container %s...", container.Name)
	containerMetrics := &MetricSet{
		Labels:         this.cloneLabels(podLabels),
		MetricValues:   map[string]MetricValue{},
		LabeledMetrics: []LabeledMetric{},
		CreateTime:     container.StartTime.Time,
		ScrapeTime:     this.getScrapeTime(container.CPU, container.Memory, nil),
	}
	containerMetrics.Labels[LabelMetricSetType.Key] = MetricSetTypePodContainer
	if isSystemContainer {
		containerMetrics.Labels[LabelContainerName.Key] = this.getSystemContainerName(container)
	} else {
		containerMetrics.Labels[LabelContainerName.Key] = container.Name
	}

	this.decodeUptime(containerMetrics, container.StartTime.Time)
	this.decodeCPUStats(containerMetrics, container.CPU)
	this.decodeMemoryStats(containerMetrics, container.Memory)
	this.decodeFsStats(containerMetrics, RootFsKey, container.Rootfs)
	this.decodeFsStats(containerMetrics, LogsKey, container.Logs)
	this.decodeUserDefinedMetrics(containerMetrics, container.UserDefinedMetrics)

	return containerMetrics
}

func (this *summaryMetricsSource) decodeUptime(metrics *MetricSet, startTime time.Time) {
	if startTime.IsZero() {
		glog.V(9).Infof("missing start time!")
		return
	}

	uptime := uint64(time.Since(startTime).Nanoseconds() / time.Millisecond.Nanoseconds())
	this.addIntMetric(metrics, &MetricUptime, &uptime)
}

func (this *summaryMetricsSource) decodeCPUStats(metrics *MetricSet, cpu *stats.CPUStats) {
	if cpu == nil {
		glog.V(9).Infof("missing cpu usage metric!")
		return
	}

	this.addIntMetric(metrics, &MetricCpuUsage, cpu.UsageCoreNanoSeconds)
}

func (this *summaryMetricsSource) decodeMemoryStats(metrics *MetricSet, memory *stats.MemoryStats) {
	if memory == nil {
		glog.V(9).Infof("missing memory metrics!")
		return
	}

	this.addIntMetric(metrics, &MetricMemoryUsage, memory.UsageBytes)
	this.addIntMetric(metrics, &MetricMemoryWorkingSet, memory.WorkingSetBytes)
	this.addIntMetric(metrics, &MetricMemoryPageFaults, memory.PageFaults)
	this.addIntMetric(metrics, &MetricMemoryMajorPageFaults, memory.MajorPageFaults)
}

func (this *summaryMetricsSource) decodeNetworkStats(metrics *MetricSet, network *stats.NetworkStats) {
	if network == nil {
		glog.V(9).Infof("missing network metrics!")
		return
	}

	this.addIntMetric(metrics, &MetricNetworkRx, network.RxBytes)
	this.addIntMetric(metrics, &MetricNetworkRxErrors, network.RxErrors)
	this.addIntMetric(metrics, &MetricNetworkTx, network.TxBytes)
	this.addIntMetric(metrics, &MetricNetworkTxErrors, network.TxErrors)
}

func (this *summaryMetricsSource) decodeFsStats(metrics *MetricSet, fsKey string, fs *stats.FsStats) {
	if fs == nil {
		glog.V(9).Infof("missing fs metrics!")
		return
	}

	fsLabels := map[string]string{LabelResourceID.Key: fsKey}
	this.addLabeledIntMetric(metrics, &MetricFilesystemUsage, fsLabels, fs.UsedBytes)
	this.addLabeledIntMetric(metrics, &MetricFilesystemLimit, fsLabels, fs.CapacityBytes)
	this.addLabeledIntMetric(metrics, &MetricFilesystemAvailable, fsLabels, fs.AvailableBytes)
}

func (this *summaryMetricsSource) decodeUserDefinedMetrics(metrics *MetricSet, udm []stats.UserDefinedMetric) {
	for _, metric := range udm {
		mv := MetricValue{}
		switch metric.Type {
		case stats.MetricGauge:
			mv.MetricType = MetricGauge
		case stats.MetricCumulative:
			mv.MetricType = MetricCumulative
		case stats.MetricDelta:
			mv.MetricType = MetricDelta
		default:
			glog.V(4).Infof("Skipping %s: unknown custom metric type: %v", metric.Name, metric.Type)
			continue
		}

		// TODO: Handle double-precision values.
		mv.ValueType = ValueFloat
		mv.FloatValue = float32(metric.Value)

		metrics.MetricValues[CustomMetricPrefix+metric.Name] = mv
	}
}

func (this *summaryMetricsSource) getScrapeTime(cpu *stats.CPUStats, memory *stats.MemoryStats, network *stats.NetworkStats) time.Time {
	// Assume CPU, memory and network scrape times are the same.
	switch {
	case cpu != nil && !cpu.Time.IsZero():
		return cpu.Time.Time
	case memory != nil && !memory.Time.IsZero():
		return memory.Time.Time
	case network != nil && !network.Time.IsZero():
		return network.Time.Time
	default:
		return time.Time{}
	}
}

// addIntMetric is a convenience method for adding the metric and value to the metric set.
func (this *summaryMetricsSource) addIntMetric(metrics *MetricSet, metric *Metric, value *uint64) {
	if value == nil {
		glog.V(9).Infof("skipping metric %s because the value was nil", metric.Name)
		return
	}
	val := MetricValue{
		ValueType:  ValueInt64,
		MetricType: metric.Type,
		IntValue:   int64(*value),
	}
	metrics.MetricValues[metric.Name] = val
}

// addLabeledIntMetric is a convenience method for adding the labeled metric and value to the metric set.
func (this *summaryMetricsSource) addLabeledIntMetric(metrics *MetricSet, metric *Metric, labels map[string]string, value *uint64) {
	if value == nil {
		glog.V(9).Infof("skipping labeled metric %s (%v) because the value was nil", metric.Name, labels)
		return
	}

	val := LabeledMetric{
		Name:   metric.Name,
		Labels: labels,
		MetricValue: MetricValue{
			ValueType:  ValueInt64,
			MetricType: metric.Type,
			IntValue:   int64(*value),
		},
	}
	metrics.LabeledMetrics = append(metrics.LabeledMetrics, val)
}

// Translate system container names to the legacy names for backwards compatibility.
func (this *summaryMetricsSource) getSystemContainerName(c *stats.ContainerStats) string {
	if legacyName, ok := systemNameMap[c.Name]; ok {
		return legacyName
	}
	return c.Name
}

// TODO: The summaryProvider duplicates a lot of code from kubeletProvider, and should be refactored.
type summaryProvider struct {
	nodeLister    v1listers.NodeLister
	reflector     *cache.Reflector
	kubeletClient *kubelet.KubeletClient
}

func (this *summaryProvider) GetMetricsSources() []MetricsSource {
	sources := []MetricsSource{}
	nodes, err := this.nodeLister.List(labels.Everything())
	if err != nil {
		glog.Errorf("error while listing nodes: %v", err)
		return sources
	}

	for _, node := range nodes {
		info, err := this.getNodeInfo(node)
		if err != nil {
			glog.Errorf("%v", err)
			continue
		}
		sources = append(sources, NewSummaryMetricsSource(info, this.kubeletClient))
	}
	return sources
}

func (this *summaryProvider) getNodeInfo(node *kube_api.Node) (NodeInfo, error) {
	for _, c := range node.Status.Conditions {
		if c.Type == kube_api.NodeReady && c.Status != kube_api.ConditionTrue {
			return NodeInfo{}, fmt.Errorf("Node %v is not ready", node.Name)
		}
	}
	info := NodeInfo{
		NodeName: node.Name,
		HostName: node.Name,
		HostID:   node.Spec.ExternalID,
		Host: kubelet.Host{
			Port: this.kubeletClient.GetPort(),
		},
		KubeletVersion: node.Status.NodeInfo.KubeletVersion,
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type == kube_api.NodeHostName && addr.Address != "" {
			info.HostName = addr.Address
		}
		if addr.Type == kube_api.NodeInternalIP && addr.Address != "" {
			info.IP = addr.Address
		}
		if addr.Type == kube_api.NodeLegacyHostIP && addr.Address != "" && info.IP == "" {
			info.IP = addr.Address
		}
	}

	if info.IP == "" {
		return info, fmt.Errorf("Node %v has no valid hostname and/or IP address: %v %v", node.Name, info.HostName, info.IP)
	}

	return info, nil
}

func NewSummaryProvider(uri *url.URL) (MetricsSourceProvider, error) {
	// create clients
	kubeConfig, kubeletConfig, err := kubelet.GetKubeConfigs(uri)
	if err != nil {
		return nil, err
	}
	kubeClient := kube_client.NewForConfigOrDie(kubeConfig)
	kubeletClient, err := kubelet.NewKubeletClient(kubeletConfig)
	if err != nil {
		return nil, err
	}
	// watch nodes
	nodeLister, reflector, _ := util.GetNodeLister(kubeClient)

	return &summaryProvider{
		nodeLister:    nodeLister,
		reflector:     reflector,
		kubeletClient: kubeletClient,
	}, nil
}
