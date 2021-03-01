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

package tester

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/octago/sflags/gen/gpflag"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/kubetest2/pkg/testers/ginkgo"

	api "k8s.io/kops/pkg/apis/kops/v1alpha2"
	"k8s.io/kops/tests/e2e/pkg/kops"
)

// Tester wraps kubetest2's ginkgo tester with additional functionality
type Tester struct {
	*ginkgo.Tester

	kopsCluster        *api.Cluster
	kopsInstanceGroups []*api.InstanceGroup
}

func (t *Tester) pretestSetup() error {
	kubectlPath, err := t.AcquireKubectl()
	if err != nil {
		return fmt.Errorf("failed to get kubectl package from published releases: %s", err)
	}

	existingPath := os.Getenv("PATH")
	newPath := fmt.Sprintf("%v:%v", filepath.Dir(kubectlPath), existingPath)
	klog.Info("Setting PATH=", newPath)
	return os.Setenv("PATH", newPath)
}

// parseKubeconfig will get the current kubeconfig, and extract the specified field by jsonpath.
func parseKubeconfig(jsonPath string) (string, error) {
	args := []string{
		"kubectl", "config", "view", "--minify", "-o", "jsonpath={" + jsonPath + "}",
	}
	c := exec.Command(args[0], args[1:]...)
	var stdout bytes.Buffer
	c.Stdout = &stdout
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		klog.Warningf("failed to run %s; stderr=%s", strings.Join(args, " "), stderr.String())
		return "", fmt.Errorf("error querying current config from kubectl: %w", err)
	}

	s := strings.TrimSpace(stdout.String())
	if s == "" {
		return "", fmt.Errorf("kubeconfig did not contain " + jsonPath)
	}
	return s, nil
}

// The --host flag was required in the kubernetes e2e tests, until https://github.com/kubernetes/kubernetes/pull/87030
// We can likely drop this when we drop support / testing for k8s 1.17
func (t *Tester) addHostFlag() error {
	server, err := parseKubeconfig(".clusters[0].cluster.server")
	if err != nil {
		return err
	}
	klog.Infof("Adding --host=%s", server)
	t.TestArgs += " --host=" + server
	return nil
}

// hasFlag detects if the specified flag has been passed in the args
func hasFlag(args string, flag string) bool {
	for _, arg := range strings.Split(args, " ") {
		if !strings.HasPrefix(arg, "-") {
			continue
		}

		arg = strings.TrimLeft(arg, "-")
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

func (t *Tester) getKopsCluster() (*api.Cluster, error) {
	if t.kopsCluster != nil {
		return t.kopsCluster, nil
	}

	currentContext, err := parseKubeconfig(".current-context")
	if err != nil {
		return nil, err
	}

	kopsClusterName := currentContext

	cluster, err := kops.GetCluster(kopsClusterName)
	if err != nil {
		return nil, err
	}
	t.kopsCluster = cluster

	return cluster, nil

}

func (t *Tester) getKopsInstanceGroups() ([]*api.InstanceGroup, error) {
	if t.kopsInstanceGroups != nil {
		return t.kopsInstanceGroups, nil
	}

	cluster, err := t.getKopsCluster()
	if err != nil {
		return nil, err
	}

	igs, err := kops.GetInstanceGroups(cluster.Name)
	if err != nil {
		return nil, err
	}
	t.kopsInstanceGroups = igs

	return igs, nil

}
func (t *Tester) addProviderFlag() error {
	if hasFlag(t.TestArgs, "provider") {
		return nil
	}

	cluster, err := t.getKopsCluster()
	if err != nil {
		return err
	}

	provider := ""
	switch cluster.Spec.CloudProvider {
	case "aws", "gce":
		provider = cluster.Spec.CloudProvider
	case "digitalocean":
	default:
		klog.Warningf("unhandled cluster.spec.cloudProvider %q for determining ginkgo Provider", cluster.Spec.CloudProvider)
	}

	klog.Infof("Setting --provider=%s", provider)
	t.TestArgs += " --provider=" + provider
	return nil
}

func (t *Tester) addZoneFlag() error {
	// gce-zone is indeed used for AWS as well!
	if hasFlag(t.TestArgs, "gce-zone") {
		return nil
	}

	zoneNames, err := t.getZones()
	if err != nil {
		return err
	}

	// gce-zone only expects one zone, we just pass the first one
	zone := zoneNames[0]
	klog.Infof("Setting --gce-zone=%s", zone)
	t.TestArgs += " --gce-zone=" + zone

	// TODO: Pass the new gce-zones flag for 1.21 with all zones?

	return nil
}

func (t *Tester) addMultiZoneFlag() error {
	if hasFlag(t.TestArgs, "gce-multizone") {
		return nil
	}

	zoneNames, err := t.getZones()
	if err != nil {
		return err
	}

	klog.Infof("Setting --gce-multizone=%t", len(zoneNames) > 1)
	t.TestArgs += fmt.Sprintf(" --gce-multizone=%t", len(zoneNames) > 1)

	return nil
}

func (t *Tester) addRegionFlag() error {
	// gce-zone is used for other cloud providers as well
	if hasFlag(t.TestArgs, "gce-region") {
		return nil
	}

	cluster, err := t.getKopsCluster()
	if err != nil {
		return err
	}

	// We don't explicitly set the provider's region in the spec so we need to extract it from vairous fields
	var region string
	switch cluster.Spec.CloudProvider {
	case "aws":
		zone := cluster.Spec.Subnets[0].Zone
		region = zone[:len(zone)-1]
	case "gce":
		region = cluster.Spec.Subnets[0].Region
	default:
		klog.Warningf("unhandled region detection for cloud provider: %v", cluster.Spec.CloudProvider)
	}

	klog.Infof("Setting --gce-region=%s", region)
	t.TestArgs += " --gce-region=" + region
	return nil
}

func (t *Tester) addClusterTagFlag() error {
	if hasFlag(t.TestArgs, "cluster-tag") {
		return nil
	}

	cluster, err := t.getKopsCluster()
	if err != nil {
		return err
	}

	clusterName := cluster.ObjectMeta.Name
	klog.Infof("Setting --cluster-tag=%s", clusterName)
	t.TestArgs += " --cluster-tag=" + clusterName

	return nil
}

func (t *Tester) addProjectFlag() error {
	if hasFlag(t.TestArgs, "gce-project") {
		return nil
	}

	cluster, err := t.getKopsCluster()
	if err != nil {
		return err
	}

	projectID := cluster.Spec.Project
	if projectID == "" {
		return nil
	}
	klog.Infof("Setting --gce-project=%s", projectID)
	t.TestArgs += " --gce-project=" + projectID

	return nil
}

func (t *Tester) getZones() ([]string, error) {
	cluster, err := t.getKopsCluster()
	if err != nil {
		return nil, err
	}

	igs, err := t.getKopsInstanceGroups()
	if err != nil {
		return nil, err
	}

	zones := sets.NewString()
	// Gather zones on AWS
	for _, subnet := range cluster.Spec.Subnets {
		if subnet.Zone != "" {
			zones.Insert(subnet.Zone)
		}
	}
	// Gather zones on GCE
	for _, ig := range igs {
		for _, zone := range ig.Spec.Zones {
			zones.Insert(zone)
		}
	}
	zoneNames := zones.List()

	if len(zoneNames) == 0 {
		klog.Warningf("no zones found in instance groups")
		return nil, nil
	}
	return zoneNames, nil
}

func (t *Tester) execute() error {
	fs, err := gpflag.Parse(t)
	if err != nil {
		return fmt.Errorf("failed to initialize tester: %v", err)
	}

	help := fs.BoolP("help", "h", false, "")
	if err := fs.Parse(os.Args); err != nil {
		return fmt.Errorf("failed to parse flags: %v", err)
	}

	if *help {
		fs.SetOutput(os.Stdout)
		fs.PrintDefaults()
		return nil
	}

	if err := t.pretestSetup(); err != nil {
		return err
	}

	if err := t.addHostFlag(); err != nil {
		return err
	}

	if err := t.addProviderFlag(); err != nil {
		return err
	}

	if err := t.addZoneFlag(); err != nil {
		return err
	}

	if err := t.addClusterTagFlag(); err != nil {
		return err
	}

	if err := t.addRegionFlag(); err != nil {
		return err
	}

	if err := t.addMultiZoneFlag(); err != nil {
		return err
	}

	if err := t.addProjectFlag(); err != nil {
		return err
	}

	return t.Test()
}

func NewDefaultTester() *Tester {
	t := &Tester{}
	t.Tester = ginkgo.NewDefaultTester()
	return t
}

func Main() {
	t := NewDefaultTester()
	if err := t.execute(); err != nil {
		klog.Fatalf("failed to run ginkgo tester: %v", err)
	}
}
