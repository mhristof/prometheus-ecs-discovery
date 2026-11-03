// Copyright 2017 Teralytics.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"gopkg.in/yaml.v3"
)

type labels struct {
	TaskArn       string `yaml:"task_arn"`
	TaskName      string `yaml:"task_name"`
	JobName       string `yaml:"job,omitempty"`
	TaskRevision  string `yaml:"task_revision"`
	TaskGroup     string `yaml:"task_group"`
	ClusterArn    string `yaml:"cluster_arn"`
	ContainerName string `yaml:"container_name"`
	ContainerArn  string `yaml:"container_arn"`
	DockerImage   string `yaml:"docker_image"`
	MetricsPath   string `yaml:"__metrics_path__,omitempty"`
	Scheme        string `yaml:"__scheme__,omitempty"`
}

// Docker label for enabling dynamic port detection
const dynamicPortLabel = "PROMETHEUS_DYNAMIC_EXPORT"

// Number of concurrent workers used to fan out AWS API calls.
const apiWorkers = 4

var cluster = flag.String("config.cluster", "", "name of the cluster to scrape")
var outFile = flag.String("config.write-to", "ecs_file_sd.yml", "path of file to write ECS service discovery information to")
var interval = flag.Duration("config.scrape-interval", 60*time.Second, "interval at which to scrape the AWS API for ECS service discovery information")
var times = flag.Int("config.scrape-times", 0, "how many times to scrape before exiting (0 = infinite)")
var roleArn = flag.String("config.role-arn", "", "ARN of the role to assume when scraping the AWS API (optional)")
var prometheusPortLabel = flag.String("config.port-label", "PROMETHEUS_EXPORTER_PORT", "Docker label to define the scrape port of the application (if missing an application won't be scraped)")
var prometheusPathLabel = flag.String("config.path-label", "PROMETHEUS_EXPORTER_PATH", "Docker label to define the scrape path of the application")
var prometheusSchemeLabel = flag.String("config.scheme-label", "PROMETHEUS_EXPORTER_SCHEME", "Docker label to define the scheme of the target application")
var prometheusFilterLabel = flag.String("config.filter-label", "", "Docker label (and optionally value) to require to scrape the application")
var prometheusServerNameLabel = flag.String("config.server-name-label", "PROMETHEUS_EXPORTER_SERVER_NAME", "Docker label to define the server name")
var prometheusJobNameLabel = flag.String("config.job-name-label", "PROMETHEUS_EXPORTER_JOB_NAME", "Docker label to define the job name")
var prometheusDynamicPortDetection = flag.Bool("config.dynamic-port-detection", false, fmt.Sprintf("If true, only tasks with the Docker label %s=1 will be scraped", dynamicPortLabel))

// ecsAPI is the subset of the ECS client used by this program.
type ecsAPI interface {
	ecs.ListClustersAPIClient
	ecs.ListTasksAPIClient
	DescribeClusters(context.Context, *ecs.DescribeClustersInput, ...func(*ecs.Options)) (*ecs.DescribeClustersOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	DescribeTaskDefinition(context.Context, *ecs.DescribeTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
	DescribeContainerInstances(context.Context, *ecs.DescribeContainerInstancesInput, ...func(*ecs.Options)) (*ecs.DescribeContainerInstancesOutput, error)
}

// ec2API is the subset of the EC2 client used by this program.
type ec2API interface {
	ec2.DescribeInstancesAPIClient
}

// exporterConfig holds the Docker label names (and filter) that
// decide which containers are exported and how.
type exporterConfig struct {
	PortLabel            string
	PathLabel            string
	SchemeLabel          string
	ServerNameLabel      string
	JobNameLabel         string
	DynamicPortDetection bool
	// FilterLabel, if non-empty, must be present on the container.
	// If HasFilterValue is set, its value must also equal FilterValue.
	FilterLabel    string
	FilterValue    string
	HasFilterValue bool
}

// newExporterConfig builds an exporterConfig from the command line flags.
func newExporterConfig() *exporterConfig {
	cfg := &exporterConfig{
		PortLabel:            *prometheusPortLabel,
		PathLabel:            *prometheusPathLabel,
		SchemeLabel:          *prometheusSchemeLabel,
		ServerNameLabel:      *prometheusServerNameLabel,
		JobNameLabel:         *prometheusJobNameLabel,
		DynamicPortDetection: *prometheusDynamicPortDetection,
	}
	cfg.FilterLabel, cfg.FilterValue, cfg.HasFilterValue = strings.Cut(*prometheusFilterLabel, "=")
	return cfg
}

// matchesFilter reports whether the Docker labels satisfy the filter.
func (cfg *exporterConfig) matchesFilter(dockerLabels map[string]string) bool {
	if cfg.FilterLabel == "" {
		return true
	}
	v, ok := dockerLabels[cfg.FilterLabel]
	if !ok {
		return false
	}
	return !cfg.HasFilterValue || v == cfg.FilterValue
}

// logError is a convenience function that decodes all possible ECS
// errors and displays them to standard error.
func logError(err error) {
	if err != nil {
		var oe *smithy.OperationError
		if errors.As(err, &oe) {
			log.Printf("failed to call service: %s, operation: %s, error: %v", oe.Service(), oe.Operation(), oe.Unwrap())
		} else {
			log.Println(err.Error())
		}
	}
}

// GetClusters retrieves the list of cluster ARNs from Amazon ECS,
// dealing with the mandatory pagination as needed.
func GetClusters(ctx context.Context, svc ecs.ListClustersAPIClient) ([]string, error) {
	var clusterArns []string
	p := ecs.NewListClustersPaginator(svc, &ecs.ListClustersInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		clusterArns = append(clusterArns, page.ClusterArns...)
	}
	return clusterArns, nil
}

// AugmentedTask represents an ECS task augmented with an extra set of
// structures representing the ECS task definition and EC2 instance
// associated with the running task.
type AugmentedTask struct {
	*ecstypes.Task
	TaskDefinition *ecstypes.TaskDefinition
	EC2Instance    *ec2types.Instance
}

// PrometheusTaskInfo is the final structure that will be
// output as a Prometheus file service discovery config.
type PrometheusTaskInfo struct {
	Targets []string `yaml:"targets"`
	Labels  labels   `yaml:"labels"`
}

// ExporterInformation returns a list of []*PrometheusTaskInfo
// enumerating the IPs, ports that the task's containers exports
// to Prometheus (one per container), so long as the Docker
// labels in its corresponding container definition for the
// container in the task has a PROMETHEUS_EXPORTER_PORT
//
// Example:
//
//	...
//	        "Name": "apache",
//	        "DockerLabels": {
//	             "PROMETHEUS_EXPORTER_PORT": "1234"
//	         },
//	...
//	         "PortMappings": [
//	           {
//	             "ContainerPort": 1883,
//	             "HostPort": 0,
//	             "Protocol": "tcp"
//	           },
//	           {
//	             "ContainerPort": 1234,
//	             "HostPort": 0,
//	             "Protocol": "tcp"
//	           }
//	         ],
//	...
func (t *AugmentedTask) ExporterInformation(cfg *exporterConfig) []*PrometheusTaskInfo {
	ret := []*PrometheusTaskInfo{}
	var ip string

	if t.TaskDefinition == nil {
		return ret
	}

	if t.LaunchType != ecstypes.LaunchTypeFargate {
		if t.EC2Instance == nil {
			return ret
		}
		instanceDNS := aws.ToString(t.EC2Instance.PrivateDnsName)
		for _, iface := range t.EC2Instance.NetworkInterfaces {
			ifaceIP := aws.ToString(iface.PrivateIpAddress)
			ifaceDNS := aws.ToString(iface.PrivateDnsName)
			if ifaceIP != "" && ifaceDNS != "" && ifaceDNS == instanceDNS {
				ip = ifaceIP
				break
			}
		}
		if ip == "" {
			return ret
		}
	}

	defs := make(map[string]ecstypes.ContainerDefinition, len(t.TaskDefinition.ContainerDefinitions))
	for _, d := range t.TaskDefinition.ContainerDefinitions {
		defs[aws.ToString(d.Name)] = d
	}

	for _, i := range t.Containers {
		d, ok := defs[aws.ToString(i.Name)]
		if !ok {
			// No container definition matches this container, so it
			// cannot be exported.
			continue
		}

		if !cfg.matchesFilter(d.DockerLabels) {
			continue
		}

		targetIP := ip
		var hostPort int32
		if cfg.DynamicPortDetection {
			v, ok := d.DockerLabels[dynamicPortLabel]
			if !ok || v != "1" {
				// Nope, no Prometheus-exported port in this container def.
				// This container is no good. We continue.
				continue
			}

			if len(i.NetworkBindings) != 1 {
				// Dynamic port mapping is only supported with a single binding.
				// Otherwise, how would we know which port to use?
				continue
			}

			hostPort = aws.ToInt32(i.NetworkBindings[0].HostPort)
		} else {
			v, ok := d.DockerLabels[cfg.PortLabel]
			if !ok {
				// Nope, no Prometheus-exported port in this container def.
				// This container is no good.  We continue.
				continue
			}

			exporterPort, err := strconv.Atoi(v)
			if err != nil || exporterPort <= 0 || exporterPort > 65535 {
				// This container has an invalid port definition.
				// This container is no good.  We continue.
				continue
			}

			if len(i.NetworkBindings) > 0 {
				for _, nb := range i.NetworkBindings {
					if nb.ContainerPort != nil && int(*nb.ContainerPort) == exporterPort {
						hostPort = aws.ToInt32(nb.HostPort)
					}
				}
			} else {
				for _, ni := range i.NetworkInterfaces {
					if addr := aws.ToString(ni.PrivateIpv4Address); addr != "" {
						targetIP = addr
					}
				}
				hostPort = int32(exporterPort)
			}
		}

		if hostPort == 0 {
			// This container has network bindings but none have a container port matching the exporter port.
			// Since the host port is mandatory for the generated Prometheus config and host port 0 does
			// not make sense, this container will be skipped.
			continue
		}

		host := targetIP
		if exporterServerName, ok := d.DockerLabels[cfg.ServerNameLabel]; ok {
			host = strings.TrimRight(exporterServerName, "/")
		}
		if host == "" {
			// Neither a server name nor an IP address is known, so
			// there is nothing Prometheus could scrape.
			continue
		}

		labels := labels{
			TaskArn:       aws.ToString(t.TaskArn),
			TaskName:      aws.ToString(t.TaskDefinition.Family),
			JobName:       d.DockerLabels[cfg.JobNameLabel],
			TaskRevision:  fmt.Sprintf("%d", t.TaskDefinition.Revision),
			TaskGroup:     aws.ToString(t.Group),
			ClusterArn:    aws.ToString(t.ClusterArn),
			ContainerName: aws.ToString(i.Name),
			ContainerArn:  aws.ToString(i.ContainerArn),
			DockerImage:   aws.ToString(d.Image),
		}

		if exporterPath, ok := d.DockerLabels[cfg.PathLabel]; ok {
			labels.MetricsPath = exporterPath
		}

		if scheme, ok := d.DockerLabels[cfg.SchemeLabel]; ok {
			labels.Scheme = scheme
		}

		ret = append(ret, &PrometheusTaskInfo{
			Targets: []string{fmt.Sprintf("%s:%d", host, hostPort)},
			Labels:  labels,
		})
	}
	return ret
}

// AddTaskDefinitionsOfTasks adds to each Task the TaskDefinition
// corresponding to it.
func AddTaskDefinitionsOfTasks(ctx context.Context, svc ecsAPI, taskList []*AugmentedTask) ([]*AugmentedTask, error) {
	task2def := make(map[string]*ecstypes.TaskDefinition)
	for _, task := range taskList {
		if task.TaskDefinitionArn != nil {
			task2def[*task.TaskDefinitionArn] = nil
		}
	}

	type result struct {
		out *ecs.DescribeTaskDefinitionOutput
		err error
	}
	jobs := make(chan string, len(task2def))
	results := make(chan result, len(task2def))

	for w := 0; w < apiWorkers; w++ {
		go func() {
			for arn := range jobs {
				out, err := svc.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(arn)})
				results <- result{out, err}
			}
		}()
	}

	for arn := range task2def {
		jobs <- arn
	}
	close(jobs)

	var err error
	for range task2def {
		res := <-results
		if res.err != nil {
			err = res.err
			log.Printf("Error describing task definition: %s", err)
		} else if res.out.TaskDefinition != nil {
			task2def[aws.ToString(res.out.TaskDefinition.TaskDefinitionArn)] = res.out.TaskDefinition
		}
	}
	if err != nil {
		return nil, err
	}

	for _, task := range taskList {
		task.TaskDefinition = task2def[aws.ToString(task.TaskDefinitionArn)]
	}
	return taskList, nil
}

// SplitArray splits given array into chunks, it's usefull
// because AWS API has limits on number of elements you can
// submit via one call.
func SplitArray(a []string, size int) [][]string {
	var splitted [][]string
	for i := 0; i < len(a); i += size {
		end := i + size
		if end > len(a) {
			end = len(a)
		}
		splitted = append(splitted, a[i:end])
	}
	return splitted
}

// DescribeInstances describes a list of EC2 instances, in chunks
// of 100 IDs per call.
func DescribeInstances(ctx context.Context, svc ec2.DescribeInstancesAPIClient, instanceIds []string) ([]ec2types.Instance, error) {
	result := []ec2types.Instance{}
	for _, chunkedInstanceIds := range SplitArray(instanceIds, 100) {
		p := ec2.NewDescribeInstancesPaginator(svc, &ec2.DescribeInstancesInput{
			InstanceIds: chunkedInstanceIds,
		})
		for p.HasMorePages() {
			output, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			log.Printf("Described %d EC2 reservations", len(output.Reservations))
			for _, rsv := range output.Reservations {
				result = append(result, rsv.Instances...)
			}
		}
	}
	return result, nil
}

// AddContainerInstancesToTasks adds to each Task the EC2 instance
// running its containers.
func AddContainerInstancesToTasks(ctx context.Context, svc ecsAPI, svcec2 ec2API, taskList []*AugmentedTask) ([]*AugmentedTask, error) {

	clusterArnToContainerInstancesArns := make(map[string]map[string]*ecstypes.ContainerInstance)
	for _, task := range taskList {
		if task.ContainerInstanceArn != nil && task.ClusterArn != nil {
			if _, ok := clusterArnToContainerInstancesArns[*task.ClusterArn]; !ok {
				clusterArnToContainerInstancesArns[*task.ClusterArn] = make(map[string]*ecstypes.ContainerInstance)
			}
			clusterArnToContainerInstancesArns[*task.ClusterArn][*task.ContainerInstanceArn] = nil
		}
	}

	instanceIDToEC2Instance := make(map[string]*ec2types.Instance)
	for clusterArn, containerInstancesArns := range clusterArnToContainerInstancesArns {
		keys := make([]string, 0, len(containerInstancesArns))
		for k := range containerInstancesArns {
			keys = append(keys, k)
		}

		for _, chunkedKeys := range SplitArray(keys, 100) {
			input := &ecs.DescribeContainerInstancesInput{
				Cluster:            aws.String(clusterArn),
				ContainerInstances: chunkedKeys,
			}
			output, err := svc.DescribeContainerInstances(ctx, input)
			if err != nil {
				return nil, err
			}

			if len(output.Failures) > 0 {
				log.Printf("Described %d failures in cluster %s", len(output.Failures), clusterArn)
			}
			for _, ci := range output.ContainerInstances {
				if ci.ContainerInstanceArn == nil || ci.Ec2InstanceId == nil {
					continue
				}
				cInst := ci
				clusterArnToContainerInstancesArns[clusterArn][*cInst.ContainerInstanceArn] = &cInst
				instanceIDToEC2Instance[*cInst.Ec2InstanceId] = nil
			}
		}
	}
	if len(instanceIDToEC2Instance) == 0 {
		return taskList, nil
	}

	keys := make([]string, 0, len(instanceIDToEC2Instance))
	for id := range instanceIDToEC2Instance {
		keys = append(keys, id)
	}

	instances, err := DescribeInstances(ctx, svcec2, keys)
	if err != nil {
		return nil, err
	}

	for _, i := range instances {
		if i.InstanceId == nil {
			continue
		}
		inst := i
		instanceIDToEC2Instance[*i.InstanceId] = &inst
	}

	for _, task := range taskList {
		if task.ContainerInstanceArn != nil && task.ClusterArn != nil {
			containerInstance := clusterArnToContainerInstancesArns[*task.ClusterArn][*task.ContainerInstanceArn]
			if containerInstance == nil {
				log.Printf("Cannot find container instance %s in cluster %s", *task.ContainerInstanceArn, *task.ClusterArn)
				continue
			}
			instance := instanceIDToEC2Instance[*containerInstance.Ec2InstanceId]
			if instance == nil {
				log.Printf("Cannot find EC2 instance %s", *containerInstance.Ec2InstanceId)
				continue
			}
			task.EC2Instance = instance
		}
	}

	return taskList, nil
}

// getTasksOfCluster returns the running tasks of a single cluster.
func getTasksOfCluster(ctx context.Context, svc ecsAPI, clusterArn string) ([]ecstypes.Task, error) {
	var tasks []ecstypes.Task
	p := ecs.NewListTasksPaginator(svc, &ecs.ListTasksInput{
		Cluster: aws.String(clusterArn),
	})
	for p.HasMorePages() {
		output, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing tasks of cluster %s: %w", clusterArn, err)
		}
		if len(output.TaskArns) == 0 {
			continue
		}
		log.Printf("Inspected cluster %s, found %d tasks", clusterArn, len(output.TaskArns))
		descOutput, err := svc.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: aws.String(clusterArn),
			Tasks:   output.TaskArns,
		})
		if err != nil {
			return nil, fmt.Errorf("describing tasks of cluster %s: %w", clusterArn, err)
		}
		log.Printf("Described %d tasks in cluster %s", len(descOutput.Tasks), clusterArn)
		if len(descOutput.Failures) > 0 {
			log.Printf("Described %d failures in cluster %s", len(descOutput.Failures), clusterArn)
		}
		for _, task := range descOutput.Tasks {
			// ListTasks returns tasks whose desired status is RUNNING,
			// which includes tasks that are still being provisioned
			// and may not have an IP address yet.
			if aws.ToString(task.LastStatus) != "RUNNING" {
				continue
			}
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

// GetTasksOfClusters returns the running tasks in a list of Clusters.
func GetTasksOfClusters(ctx context.Context, svc ecsAPI, clusterArns []string) ([]ecstypes.Task, error) {
	type result struct {
		tasks []ecstypes.Task
		err   error
	}
	jobs := make(chan string, len(clusterArns))
	results := make(chan result, len(clusterArns))

	for w := 0; w < apiWorkers; w++ {
		go func() {
			for clusterArn := range jobs {
				tasks, err := getTasksOfCluster(ctx, svc, clusterArn)
				results <- result{tasks, err}
			}
		}()
	}

	for _, clusterArn := range clusterArns {
		jobs <- clusterArn
	}
	close(jobs)

	tasks := []ecstypes.Task{}
	var err error
	for range clusterArns {
		res := <-results
		if res.err != nil {
			err = res.err
			continue
		}
		tasks = append(tasks, res.tasks...)
	}
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

// GetAugmentedTasks gets the fully AugmentedTasks running on
// a list of Clusters.
func GetAugmentedTasks(ctx context.Context, svc ecsAPI, svcec2 ec2API, clusterArns []string) ([]*AugmentedTask, error) {
	simpleTasks, err := GetTasksOfClusters(ctx, svc, clusterArns)
	if err != nil {
		return nil, err
	}

	tasks := []*AugmentedTask{}
	for i := 0; i < len(simpleTasks); i++ {
		tasks = append(tasks, &AugmentedTask{&simpleTasks[i], nil, nil})
	}
	tasks, err = AddTaskDefinitionsOfTasks(ctx, svc, tasks)
	if err != nil {
		return nil, err
	}

	tasks, err = AddContainerInstancesToTasks(ctx, svc, svcec2, tasks)
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

// marshalTargets renders the targets as a Prometheus file_sd YAML
// document, using the same 2-space indentation as yaml.v2 did.
func marshalTargets(infos []*PrometheusTaskInfo) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(infos); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes data to a temporary file next to path and
// renames it into place, so that Prometheus never reads a partially
// written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// resolveClusters returns the ARNs of the clusters to scrape: the
// one named by -config.cluster, or all clusters otherwise.
func resolveClusters(ctx context.Context, svc ecsAPI, name string) ([]string, error) {
	if name == "" {
		return GetClusters(ctx, svc)
	}
	res, err := svc.DescribeClusters(ctx, &ecs.DescribeClustersInput{
		Clusters: []string{name},
	})
	if err != nil {
		return nil, err
	}
	if len(res.Clusters) == 0 || res.Clusters[0].ClusterArn == nil {
		return nil, fmt.Errorf("%s cluster not found", name)
	}
	return []string{*res.Clusters[0].ClusterArn}, nil
}

// work performs a single discovery cycle and writes the result to outFile.
func work(ctx context.Context, svc ecsAPI, svcec2 ec2API, cfg *exporterConfig) error {
	clusterArns, err := resolveClusters(ctx, svc, *cluster)
	if err != nil {
		return err
	}

	tasks, err := GetAugmentedTasks(ctx, svc, svcec2, clusterArns)
	if err != nil {
		return err
	}
	infos := []*PrometheusTaskInfo{}
	for _, t := range tasks {
		infos = append(infos, t.ExporterInformation(cfg)...)
	}
	m, err := marshalTargets(infos)
	if err != nil {
		return err
	}
	log.Printf("Writing %d discovered exporters to %s", len(infos), *outFile)
	return writeFileAtomic(*outFile, m, 0644)
}

func main() {
	flag.Parse()

	awsConfig, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		logError(err)
		os.Exit(1)
	}

	if *roleArn != "" {
		// Assume role
		stsSvc := sts.NewFromConfig(awsConfig)
		awsConfig.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(stsSvc, *roleArn))
	}

	// Initialise AWS Service clients
	svc := ecs.NewFromConfig(awsConfig)
	svcec2 := ec2.NewFromConfig(awsConfig)
	cfg := newExporterConfig()

	failed := false
	s := time.NewTimer(1 * time.Millisecond)
	t := time.NewTicker(*interval)
	n := *times
	for {
		select {
		case <-s.C:
		case <-t.C:
		}
		// Bound each cycle so that a hung API call cannot stall discovery forever.
		ctx, cancel := context.WithTimeout(context.Background(), *interval)
		if err := work(ctx, svc, svcec2, cfg); err != nil {
			logError(err)
			failed = true
		}
		cancel()
		n = n - 1
		if *times > 0 && n == 0 {
			break
		}
	}
	if failed {
		os.Exit(1)
	}
}
