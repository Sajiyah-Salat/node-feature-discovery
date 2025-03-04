/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nfdtopologyupdater

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"golang.org/x/net/context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/node-feature-discovery/pkg/apihelper"
	"sigs.k8s.io/node-feature-discovery/pkg/podres"
	"sigs.k8s.io/node-feature-discovery/pkg/resourcemonitor"
	"sigs.k8s.io/node-feature-discovery/pkg/topologypolicy"
	"sigs.k8s.io/node-feature-discovery/pkg/utils"
	"sigs.k8s.io/node-feature-discovery/pkg/version"
	"sigs.k8s.io/yaml"
)

const (
	// TopologyManagerPolicyAttributeName represents an attribute which defines Topology Manager Policy
	TopologyManagerPolicyAttributeName = "topologyManagerPolicy"
	// TopologyManagerScopeAttributeName represents an attribute which defines Topology Manager Policy Scope
	TopologyManagerScopeAttributeName = "topologyManagerScope"
)

// Args are the command line arguments
type Args struct {
	NoPublish      bool
	Oneshot        bool
	KubeConfigFile string
	ConfigFile     string

	Klog map[string]*utils.KlogFlagVal
}

// NFDConfig contains the configuration settings of NFDTopologyUpdater.
type NFDConfig struct {
	ExcludeList map[string][]string
}

type NfdTopologyUpdater interface {
	Run() error
	Stop()
}

type staticNodeInfo struct {
	nodeName string
	tmPolicy string
	tmScope  string
}

func newStaticNodeInfo(policy, scope string) staticNodeInfo {
	nodeName := utils.NodeName()
	klog.InfoS("detected kubelet Topology Manager configuration", "policy", policy, "scope", scope, "nodeName", nodeName)
	return staticNodeInfo{
		nodeName: nodeName,
		tmPolicy: policy,
		tmScope:  scope,
	}
}

type nfdTopologyUpdater struct {
	nodeInfo            staticNodeInfo
	args                Args
	apihelper           apihelper.APIHelpers
	resourcemonitorArgs resourcemonitor.Args
	stop                chan struct{} // channel for signaling stop
	configFilePath      string
	config              *NFDConfig
}

// NewTopologyUpdater creates a new NfdTopologyUpdater instance.
func NewTopologyUpdater(args Args, resourcemonitorArgs resourcemonitor.Args, policy, scope string) NfdTopologyUpdater {
	nfd := &nfdTopologyUpdater{
		args:                args,
		resourcemonitorArgs: resourcemonitorArgs,
		nodeInfo:            newStaticNodeInfo(policy, scope),
		stop:                make(chan struct{}, 1),
		config:              &NFDConfig{},
	}
	if args.ConfigFile != "" {
		nfd.configFilePath = filepath.Clean(args.ConfigFile)
	}
	return nfd
}

// Run nfdTopologyUpdater. Returns if a fatal error is encountered, or, after
// one request if OneShot is set to 'true' in the updater args.
func (w *nfdTopologyUpdater) Run() error {
	klog.Infof("Node Feature Discovery Topology Updater %s", version.Get())
	klog.Infof("NodeName: '%s'", w.nodeInfo.nodeName)

	podResClient, err := podres.GetPodResClient(w.resourcemonitorArgs.PodResourceSocketPath)
	if err != nil {
		return fmt.Errorf("failed to get PodResource Client: %w", err)
	}

	if !w.args.NoPublish {
		kubeconfig, err := apihelper.GetKubeconfig(w.args.KubeConfigFile)
		if err != nil {
			return err
		}
		w.apihelper = apihelper.K8sHelpers{Kubeconfig: kubeconfig}
	}
	if err := w.configure(); err != nil {
		return fmt.Errorf("faild to configure Node Feature Discovery Topology Updater: %w", err)
	}

	var resScan resourcemonitor.ResourcesScanner

	resScan, err = resourcemonitor.NewPodResourcesScanner(w.resourcemonitorArgs.Namespace, podResClient, w.apihelper, w.resourcemonitorArgs.PodSetFingerprint)
	if err != nil {
		return fmt.Errorf("failed to initialize ResourceMonitor instance: %w", err)
	}

	// CAUTION: these resources are expected to change rarely - if ever.
	// So we are intentionally do this once during the process lifecycle.
	// TODO: Obtain node resources dynamically from the podresource API
	// zonesChannel := make(chan v1alpha1.ZoneList)
	var zones v1alpha2.ZoneList

	excludeList := resourcemonitor.NewExcludeResourceList(w.config.ExcludeList, w.nodeInfo.nodeName)
	resAggr, err := resourcemonitor.NewResourcesAggregator(podResClient, excludeList)
	if err != nil {
		return fmt.Errorf("failed to obtain node resource information: %w", err)
	}

	klog.V(2).Infof("resAggr is: %v\n", resAggr)

	crTrigger := time.NewTicker(w.resourcemonitorArgs.SleepInterval)
	for {
		select {
		case <-crTrigger.C:
			klog.Infof("Scanning")
			scanResponse, err := resScan.Scan()
			utils.KlogDump(1, "podResources are", "  ", scanResponse.PodResources)
			if err != nil {
				klog.Warningf("Scan failed: %v", err)
				continue
			}
			zones = resAggr.Aggregate(scanResponse.PodResources)
			utils.KlogDump(1, "After aggregating resources identified zones are", "  ", zones)
			if !w.args.NoPublish {
				if err = w.updateNodeResourceTopology(zones, scanResponse); err != nil {
					return err
				}
			}

			if w.args.Oneshot {
				return nil
			}

		case <-w.stop:
			klog.Infof("shutting down nfd-topology-updater")
			return nil
		}
	}

}

// Stop NFD Topology Updater
func (w *nfdTopologyUpdater) Stop() {
	select {
	case w.stop <- struct{}{}:
	default:
	}
}

func (w *nfdTopologyUpdater) updateNodeResourceTopology(zoneInfo v1alpha2.ZoneList, scanResponse resourcemonitor.ScanResponse) error {
	cli, err := w.apihelper.GetTopologyClient()
	if err != nil {
		return err
	}

	nrt, err := cli.TopologyV1alpha2().NodeResourceTopologies().Get(context.TODO(), w.nodeInfo.nodeName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		nrtNew := v1alpha2.NodeResourceTopology{
			ObjectMeta: metav1.ObjectMeta{
				Name: w.nodeInfo.nodeName,
			},
			Zones:            zoneInfo,
			TopologyPolicies: []string{string(topologypolicy.DetectTopologyPolicy(w.nodeInfo.tmPolicy, w.nodeInfo.tmScope))},
			Attributes:       createTopologyAttributes(w.nodeInfo.tmPolicy, w.nodeInfo.tmScope),
		}

		updateAttributes(&nrtNew.Attributes, scanResponse.Attributes)

		_, err := cli.TopologyV1alpha2().NodeResourceTopologies().Create(context.TODO(), &nrtNew, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create NodeResourceTopology: %w", err)
		}
		return nil
	} else if err != nil {
		return err
	}

	nrtMutated := nrt.DeepCopy()
	nrtMutated.Zones = zoneInfo
	updateAttributes(&nrtMutated.Attributes, scanResponse.Attributes)

	nrtUpdated, err := cli.TopologyV1alpha2().NodeResourceTopologies().Update(context.TODO(), nrtMutated, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update NodeResourceTopology: %w", err)
	}
	utils.KlogDump(4, "CR instance updated resTopo:", "  ", nrtUpdated)
	return nil
}

func (w *nfdTopologyUpdater) configure() error {
	if w.configFilePath == "" {
		klog.Warningf("file path for nfd-topology-updater conf file is empty")
		return nil
	}

	b, err := os.ReadFile(w.configFilePath)
	if err != nil {
		// config is optional
		if os.IsNotExist(err) {
			klog.Warningf("couldn't find conf file under %v", w.configFilePath)
			return nil
		}
		return err
	}

	err = yaml.Unmarshal(b, w.config)
	if err != nil {
		return fmt.Errorf("failed to parse configuration file %q: %w", w.configFilePath, err)
	}
	klog.Infof("configuration file %q parsed:\n %v", w.configFilePath, w.config)
	return nil
}

func createTopologyAttributes(policy string, scope string) v1alpha2.AttributeList {
	return v1alpha2.AttributeList{
		{
			Name:  TopologyManagerPolicyAttributeName,
			Value: policy,
		},
		{
			Name:  TopologyManagerScopeAttributeName,
			Value: scope,
		},
	}
}

func updateAttribute(attrList *v1alpha2.AttributeList, attrInfo v1alpha2.AttributeInfo) {
	if attrList == nil {
		return
	}

	for idx := range *attrList {
		if (*attrList)[idx].Name == attrInfo.Name {
			(*attrList)[idx].Value = attrInfo.Value
			return
		}
	}
	*attrList = append(*attrList, attrInfo)
}
func updateAttributes(lhs *v1alpha2.AttributeList, rhs v1alpha2.AttributeList) {
	for _, attr := range rhs {
		updateAttribute(lhs, attr)
	}
}
