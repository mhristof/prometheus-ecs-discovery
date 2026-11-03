package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

func defaultConfig() *exporterConfig {
	return &exporterConfig{
		PortLabel:       "PROMETHEUS_EXPORTER_PORT",
		PathLabel:       "PROMETHEUS_EXPORTER_PATH",
		SchemeLabel:     "PROMETHEUS_EXPORTER_SCHEME",
		ServerNameLabel: "PROMETHEUS_EXPORTER_SERVER_NAME",
		JobNameLabel:    "PROMETHEUS_EXPORTER_JOB_NAME",
	}
}

func ec2Instance(ip string) *ec2types.Instance {
	dns := "ip-" + ip + ".ec2.internal"
	return &ec2types.Instance{
		InstanceId:     aws.String("i-1"),
		PrivateDnsName: aws.String(dns),
		NetworkInterfaces: []ec2types.InstanceNetworkInterface{
			{PrivateIpAddress: aws.String("10.9.9.9"), PrivateDnsName: aws.String("other")},
			{PrivateIpAddress: aws.String(ip), PrivateDnsName: aws.String(dns)},
		},
	}
}

func container(name string, bindings ...ecstypes.NetworkBinding) ecstypes.Container {
	return ecstypes.Container{
		Name:            aws.String(name),
		ContainerArn:    aws.String("arn:container/" + name),
		NetworkBindings: bindings,
	}
}

func binding(containerPort, hostPort int32) ecstypes.NetworkBinding {
	return ecstypes.NetworkBinding{ContainerPort: aws.Int32(containerPort), HostPort: aws.Int32(hostPort)}
}

func containerDef(name string, dockerLabels map[string]string) ecstypes.ContainerDefinition {
	return ecstypes.ContainerDefinition{
		Name:         aws.String(name),
		Image:        aws.String("image/" + name),
		DockerLabels: dockerLabels,
	}
}

func task(launchType ecstypes.LaunchType, instance *ec2types.Instance, containers []ecstypes.Container, defs ...ecstypes.ContainerDefinition) *AugmentedTask {
	return &AugmentedTask{
		Task: &ecstypes.Task{
			TaskArn:    aws.String("arn:task/1"),
			ClusterArn: aws.String("arn:cluster/c"),
			Group:      aws.String("service:svc"),
			LaunchType: launchType,
			Containers: containers,
		},
		TaskDefinition: &ecstypes.TaskDefinition{
			Family:               aws.String("family"),
			Revision:             7,
			ContainerDefinitions: defs,
		},
		EC2Instance: instance,
	}
}

func targets(infos []*PrometheusTaskInfo) []string {
	ret := []string{}
	for _, i := range infos {
		ret = append(ret, i.Targets...)
	}
	return ret
}

func TestExporterInformationTargets(t *testing.T) {
	port := map[string]string{"PROMETHEUS_EXPORTER_PORT": "9100"}
	dynamic := &exporterConfig{DynamicPortDetection: true}
	withFilter := func(cfg *exporterConfig, label, value string, hasValue bool) *exporterConfig {
		cfg.FilterLabel, cfg.FilterValue, cfg.HasFilterValue = label, value, hasValue
		return cfg
	}

	tests := []struct {
		name string
		cfg  *exporterConfig
		task *AugmentedTask
		want []string
	}{
		{
			name: "ec2 bridge mode uses host port of matching binding",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeEc2, ec2Instance("10.0.0.1"),
				[]ecstypes.Container{container("app", binding(80, 32768), binding(9100, 32769))},
				containerDef("app", port)),
			want: []string{"10.0.0.1:32769"},
		},
		{
			name: "ec2 without instance is skipped",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeEc2, nil,
				[]ecstypes.Container{container("app", binding(9100, 32769))},
				containerDef("app", port)),
			want: []string{},
		},
		{
			name: "ec2 instance without matching interface is skipped",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeEc2, &ec2types.Instance{PrivateDnsName: aws.String("x")},
				[]ecstypes.Container{container("app", binding(9100, 32769))},
				containerDef("app", port)),
			want: []string{},
		},
		{
			name: "no binding for exporter port is skipped",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeEc2, ec2Instance("10.0.0.1"),
				[]ecstypes.Container{container("app", binding(80, 32768))},
				containerDef("app", port)),
			want: []string{},
		},
		{
			name: "binding with nil ports does not panic",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeEc2, ec2Instance("10.0.0.1"),
				[]ecstypes.Container{container("app", ecstypes.NetworkBinding{}, binding(9100, 32769))},
				containerDef("app", port)),
			want: []string{"10.0.0.1:32769"},
		},
		{
			name: "missing port label is skipped",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeEc2, ec2Instance("10.0.0.1"),
				[]ecstypes.Container{container("app", binding(9100, 32769))},
				containerDef("app", nil)),
			want: []string{},
		},
		{
			name: "invalid ports are skipped",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeFargate, nil,
				[]ecstypes.Container{
					awsvpcContainer("a", "10.1.0.1"),
					awsvpcContainer("b", "10.1.0.1"),
					awsvpcContainer("c", "10.1.0.1"),
					awsvpcContainer("d", "10.1.0.1"),
				},
				containerDef("a", map[string]string{"PROMETHEUS_EXPORTER_PORT": "abc"}),
				containerDef("b", map[string]string{"PROMETHEUS_EXPORTER_PORT": "-1"}),
				containerDef("c", map[string]string{"PROMETHEUS_EXPORTER_PORT": "0"}),
				containerDef("d", map[string]string{"PROMETHEUS_EXPORTER_PORT": "70000"})),
			want: []string{},
		},
		{
			name: "fargate uses task ENI address and container port",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeFargate, nil,
				[]ecstypes.Container{awsvpcContainer("app", "10.1.0.5")},
				containerDef("app", port)),
			want: []string{"10.1.0.5:9100"},
		},
		{
			name: "fargate container without definition is skipped",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeFargate, nil,
				[]ecstypes.Container{awsvpcContainer("sidecar", "10.1.0.5"), awsvpcContainer("app", "10.1.0.5")},
				containerDef("app", port)),
			want: []string{"10.1.0.5:9100"},
		},
		{
			name: "fargate task with no container definitions does not panic",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeFargate, nil,
				[]ecstypes.Container{awsvpcContainer("app", "10.1.0.5")}),
			want: []string{},
		},
		{
			name: "fargate without address or server name is skipped",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeFargate, nil,
				[]ecstypes.Container{container("app")},
				containerDef("app", port)),
			want: []string{},
		},
		{
			name: "server name label overrides address",
			cfg:  defaultConfig(),
			task: task(ecstypes.LaunchTypeEc2, ec2Instance("10.0.0.1"),
				[]ecstypes.Container{container("app", binding(9100, 32769))},
				containerDef("app", map[string]string{
					"PROMETHEUS_EXPORTER_PORT":        "9100",
					"PROMETHEUS_EXPORTER_SERVER_NAME": "app.example.com/",
				})),
			want: []string{"app.example.com:32769"},
		},
		{
			name: "task without task definition is skipped",
			cfg:  defaultConfig(),
			task: &AugmentedTask{Task: &ecstypes.Task{LaunchType: ecstypes.LaunchTypeFargate}},
			want: []string{},
		},
		{
			name: "filter label must be present",
			cfg:  withFilter(defaultConfig(), "SCRAPE", "", false),
			task: task(ecstypes.LaunchTypeFargate, nil,
				[]ecstypes.Container{awsvpcContainer("a", "10.1.0.1"), awsvpcContainer("b", "10.1.0.2")},
				containerDef("a", map[string]string{"PROMETHEUS_EXPORTER_PORT": "9100", "SCRAPE": "anything"}),
				containerDef("b", port)),
			want: []string{"10.1.0.1:9100"},
		},
		{
			name: "filter value containing '=' must match exactly",
			cfg:  withFilter(defaultConfig(), "SCRAPE", "a=b", true),
			task: task(ecstypes.LaunchTypeFargate, nil,
				[]ecstypes.Container{awsvpcContainer("a", "10.1.0.1"), awsvpcContainer("b", "10.1.0.2")},
				containerDef("a", map[string]string{"PROMETHEUS_EXPORTER_PORT": "9100", "SCRAPE": "a=b"}),
				containerDef("b", map[string]string{"PROMETHEUS_EXPORTER_PORT": "9100", "SCRAPE": "a=c"})),
			want: []string{"10.1.0.1:9100"},
		},
		{
			name: "dynamic mode uses the single binding's host port",
			cfg:  dynamic,
			task: task(ecstypes.LaunchTypeEc2, ec2Instance("10.0.0.1"),
				[]ecstypes.Container{
					container("one", binding(8080, 32770)),
					container("two", binding(8080, 32771), binding(9100, 32772)),
					container("off", binding(8080, 32773)),
				},
				containerDef("one", map[string]string{dynamicPortLabel: "1"}),
				containerDef("two", map[string]string{dynamicPortLabel: "1"}),
				containerDef("off", map[string]string{dynamicPortLabel: "0"})),
			want: []string{"10.0.0.1:32770"},
		},
		{
			name: "dynamic mode applies the filter label",
			cfg:  withFilter(&exporterConfig{DynamicPortDetection: true}, "SCRAPE", "yes", true),
			task: task(ecstypes.LaunchTypeEc2, ec2Instance("10.0.0.1"),
				[]ecstypes.Container{container("a", binding(8080, 32770)), container("b", binding(8080, 32771))},
				containerDef("a", map[string]string{dynamicPortLabel: "1", "SCRAPE": "yes"}),
				containerDef("b", map[string]string{dynamicPortLabel: "1", "SCRAPE": "no"})),
			want: []string{"10.0.0.1:32770"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targets(tt.task.ExporterInformation(tt.cfg))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("targets = %v, want %v", got, tt.want)
			}
		})
	}
}

func awsvpcContainer(name, ip string) ecstypes.Container {
	c := container(name)
	c.NetworkInterfaces = []ecstypes.NetworkInterface{{PrivateIpv4Address: aws.String(ip)}}
	return c
}

func TestExporterInformationLabels(t *testing.T) {
	tsk := task(ecstypes.LaunchTypeEc2, ec2Instance("10.0.0.1"),
		[]ecstypes.Container{container("app", binding(9100, 32769))},
		containerDef("app", map[string]string{
			"PROMETHEUS_EXPORTER_PORT":     "9100",
			"PROMETHEUS_EXPORTER_PATH":     "/custom",
			"PROMETHEUS_EXPORTER_SCHEME":   "https",
			"PROMETHEUS_EXPORTER_JOB_NAME": "myjob",
		}))

	got := tsk.ExporterInformation(defaultConfig())
	if len(got) != 1 {
		t.Fatalf("got %d infos, want 1", len(got))
	}
	want := labels{
		TaskArn:       "arn:task/1",
		TaskName:      "family",
		JobName:       "myjob",
		TaskRevision:  "7",
		TaskGroup:     "service:svc",
		ClusterArn:    "arn:cluster/c",
		ContainerName: "app",
		ContainerArn:  "arn:container/app",
		DockerImage:   "image/app",
		MetricsPath:   "/custom",
		Scheme:        "https",
	}
	if got[0].Labels != want {
		t.Errorf("labels = %+v, want %+v", got[0].Labels, want)
	}
}

func TestExporterInformationNilTaskFields(t *testing.T) {
	tsk := task(ecstypes.LaunchTypeFargate, nil,
		[]ecstypes.Container{{Name: aws.String("app"), NetworkInterfaces: []ecstypes.NetworkInterface{{PrivateIpv4Address: aws.String("10.1.0.1")}}}},
		ecstypes.ContainerDefinition{Name: aws.String("app"), DockerLabels: map[string]string{"PROMETHEUS_EXPORTER_PORT": "9100"}})
	tsk.Group = nil
	tsk.TaskArn = nil
	tsk.ClusterArn = nil
	tsk.TaskDefinition.Family = nil

	got := targets(tsk.ExporterInformation(defaultConfig()))
	if want := []string{"10.1.0.1:9100"}; !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestNewExporterConfigFilter(t *testing.T) {
	tests := []struct {
		flag      string
		label     string
		value     string
		hasValue  bool
		labels    map[string]string
		wantMatch bool
	}{
		{"", "", "", false, nil, true},
		{"K", "K", "", false, map[string]string{"K": "x"}, true},
		{"K", "K", "", false, map[string]string{}, false},
		{"K=v", "K", "v", true, map[string]string{"K": "v"}, true},
		{"K=v", "K", "v", true, map[string]string{"K": "w"}, false},
		{"K=", "K", "", true, map[string]string{"K": ""}, true},
		{"K=", "K", "", true, map[string]string{"K": "x"}, false},
		{"K=a=b", "K", "a=b", true, map[string]string{"K": "a"}, false},
		{"K=a=b", "K", "a=b", true, map[string]string{"K": "a=b"}, true},
	}
	defer func(old string) { *prometheusFilterLabel = old }(*prometheusFilterLabel)
	for _, tt := range tests {
		*prometheusFilterLabel = tt.flag
		cfg := newExporterConfig()
		if cfg.FilterLabel != tt.label || cfg.FilterValue != tt.value || cfg.HasFilterValue != tt.hasValue {
			t.Errorf("%q: got (%q, %q, %v), want (%q, %q, %v)", tt.flag,
				cfg.FilterLabel, cfg.FilterValue, cfg.HasFilterValue, tt.label, tt.value, tt.hasValue)
		}
		if got := cfg.matchesFilter(tt.labels); got != tt.wantMatch {
			t.Errorf("%q matchesFilter(%v) = %v, want %v", tt.flag, tt.labels, got, tt.wantMatch)
		}
	}
}

func TestSplitArray(t *testing.T) {
	tests := []struct {
		in   []string
		size int
		want [][]string
	}{
		{nil, 2, nil},
		{[]string{"a"}, 2, [][]string{{"a"}}},
		{[]string{"a", "b"}, 2, [][]string{{"a", "b"}}},
		{[]string{"a", "b", "c"}, 2, [][]string{{"a", "b"}, {"c"}}},
	}
	for _, tt := range tests {
		if got := SplitArray(tt.in, tt.size); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitArray(%v, %d) = %v, want %v", tt.in, tt.size, got, tt.want)
		}
	}
}

func TestMarshalTargets(t *testing.T) {
	got, err := marshalTargets([]*PrometheusTaskInfo{{
		Targets: []string{"10.0.0.1:9100"},
		Labels: labels{
			TaskArn:       "arn:task/1",
			TaskName:      "family",
			TaskRevision:  "7",
			TaskGroup:     "service:svc",
			ClusterArn:    "arn:cluster/c",
			ContainerName: "app",
			ContainerArn:  "arn:container/app",
			DockerImage:   "image/app",
			MetricsPath:   "/custom",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `- targets:
    - 10.0.0.1:9100
  labels:
    task_arn: arn:task/1
    task_name: family
    task_revision: "7"
    task_group: service:svc
    cluster_arn: arn:cluster/c
    container_name: app
    container_arn: arn:container/app
    docker_image: image/app
    __metrics_path__: /custom
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	empty, err := marshalTargets([]*PrometheusTaskInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != "[]\n" {
		t.Errorf("empty = %q, want %q", empty, "[]\n")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ecs_file_sd.yml")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(path, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want 1 (temp file left behind?)", len(entries))
	}
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "ecs_file_sd.yml")
	if err := writeFileAtomic(path, []byte("x"), 0644); err == nil {
		t.Error("expected an error for a missing directory")
	}
}

// fakeECS is an in-memory implementation of ecsAPI.
type fakeECS struct {
	clusterPages [][]string
	// taskPages maps a cluster ARN to its pages of task ARNs.
	taskPages          map[string][][]string
	tasks              map[string]ecstypes.Task
	taskDefinitions    map[string]ecstypes.TaskDefinition
	containerInstances map[string]ecstypes.ContainerInstance
	clusters           map[string]string
	listTasksErr       error
}

func pageToken(i int) *string {
	return aws.String(string(rune('0' + i)))
}

func pageIndex(token *string) int {
	if token == nil {
		return 0
	}
	return int((*token)[0] - '0')
}

func (f *fakeECS) ListClusters(_ context.Context, in *ecs.ListClustersInput, _ ...func(*ecs.Options)) (*ecs.ListClustersOutput, error) {
	i := pageIndex(in.NextToken)
	out := &ecs.ListClustersOutput{ClusterArns: f.clusterPages[i]}
	if i+1 < len(f.clusterPages) {
		out.NextToken = pageToken(i + 1)
	}
	return out, nil
}

func (f *fakeECS) ListTasks(_ context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	if f.listTasksErr != nil {
		return nil, f.listTasksErr
	}
	pages := f.taskPages[*in.Cluster]
	if len(pages) == 0 {
		return &ecs.ListTasksOutput{}, nil
	}
	i := pageIndex(in.NextToken)
	out := &ecs.ListTasksOutput{TaskArns: pages[i]}
	if i+1 < len(pages) {
		out.NextToken = pageToken(i + 1)
	}
	return out, nil
}

func (f *fakeECS) DescribeClusters(_ context.Context, in *ecs.DescribeClustersInput, _ ...func(*ecs.Options)) (*ecs.DescribeClustersOutput, error) {
	out := &ecs.DescribeClustersOutput{}
	for _, name := range in.Clusters {
		if arn, ok := f.clusters[name]; ok {
			out.Clusters = append(out.Clusters, ecstypes.Cluster{ClusterArn: aws.String(arn)})
		} else {
			out.Failures = append(out.Failures, ecstypes.Failure{Arn: aws.String(name), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

func (f *fakeECS) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	out := &ecs.DescribeTasksOutput{}
	for _, arn := range in.Tasks {
		out.Tasks = append(out.Tasks, f.tasks[arn])
	}
	return out, nil
}

func (f *fakeECS) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	td, ok := f.taskDefinitions[*in.TaskDefinition]
	if !ok {
		return nil, errors.New("task definition not found")
	}
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &td}, nil
}

func (f *fakeECS) DescribeContainerInstances(_ context.Context, in *ecs.DescribeContainerInstancesInput, _ ...func(*ecs.Options)) (*ecs.DescribeContainerInstancesOutput, error) {
	out := &ecs.DescribeContainerInstancesOutput{}
	for _, arn := range in.ContainerInstances {
		if ci, ok := f.containerInstances[arn]; ok {
			out.ContainerInstances = append(out.ContainerInstances, ci)
		} else {
			out.Failures = append(out.Failures, ecstypes.Failure{Arn: aws.String(arn), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

// fakeEC2 is an in-memory implementation of ec2API.
type fakeEC2 struct {
	instances map[string]ec2types.Instance
	calls     [][]string
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.calls = append(f.calls, in.InstanceIds)
	rsv := ec2types.Reservation{}
	for _, id := range in.InstanceIds {
		if inst, ok := f.instances[id]; ok {
			rsv.Instances = append(rsv.Instances, inst)
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{rsv}}, nil
}

func ecsTask(arn, cluster, lastStatus string, launchType ecstypes.LaunchType, containerInstance *string, containers ...ecstypes.Container) ecstypes.Task {
	return ecstypes.Task{
		TaskArn:              aws.String(arn),
		ClusterArn:           aws.String(cluster),
		Group:                aws.String("service:svc"),
		LastStatus:           aws.String(lastStatus),
		LaunchType:           launchType,
		TaskDefinitionArn:    aws.String("arn:td/app:1"),
		ContainerInstanceArn: containerInstance,
		Containers:           containers,
	}
}

func TestGetClustersPaginates(t *testing.T) {
	svc := &fakeECS{clusterPages: [][]string{{"arn:c/1", "arn:c/2"}, {"arn:c/3"}}}
	got, err := GetClusters(context.Background(), svc)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"arn:c/1", "arn:c/2", "arn:c/3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("clusters = %v, want %v", got, want)
	}
}

func TestResolveClusters(t *testing.T) {
	svc := &fakeECS{
		clusters:     map[string]string{"prod": "arn:aws:ecs:eu-west-1:1:cluster/prod"},
		clusterPages: [][]string{{"arn:c/1"}},
	}

	got, err := resolveClusters(context.Background(), svc, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"arn:aws:ecs:eu-west-1:1:cluster/prod"}; !reflect.DeepEqual(got, want) {
		t.Errorf("clusters = %v, want %v", got, want)
	}

	if _, err := resolveClusters(context.Background(), svc, "missing"); err == nil {
		t.Error("expected an error for a missing cluster")
	}

	got, err = resolveClusters(context.Background(), svc, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"arn:c/1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("clusters = %v, want %v", got, want)
	}
}

func TestGetTasksOfClustersSkipsNonRunning(t *testing.T) {
	svc := &fakeECS{
		taskPages: map[string][][]string{
			"c1": {{"t1", "t2"}, {"t3"}},
			"c2": {},
		},
		tasks: map[string]ecstypes.Task{
			"t1": ecsTask("t1", "c1", "RUNNING", ecstypes.LaunchTypeFargate, nil),
			"t2": ecsTask("t2", "c1", "PROVISIONING", ecstypes.LaunchTypeFargate, nil),
			"t3": ecsTask("t3", "c1", "RUNNING", ecstypes.LaunchTypeFargate, nil),
		},
	}
	tasks, err := GetTasksOfClusters(context.Background(), svc, []string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tsk := range tasks {
		got = append(got, *tsk.TaskArn)
	}
	sort.Strings(got)
	if want := []string{"t1", "t3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tasks = %v, want %v", got, want)
	}
}

func TestGetTasksOfClustersError(t *testing.T) {
	svc := &fakeECS{listTasksErr: errors.New("throttled")}
	if _, err := GetTasksOfClusters(context.Background(), svc, []string{"c1", "c2", "c3", "c4", "c5"}); err == nil {
		t.Error("expected an error")
	}
}

func TestAddTaskDefinitionsOfTasksError(t *testing.T) {
	svc := &fakeECS{}
	tsk := ecsTask("t1", "c1", "RUNNING", ecstypes.LaunchTypeFargate, nil)
	if _, err := AddTaskDefinitionsOfTasks(context.Background(), svc, []*AugmentedTask{{Task: &tsk}}); err == nil {
		t.Error("expected an error for a missing task definition")
	}
}

func TestAddContainerInstancesToTasksMissingInstance(t *testing.T) {
	svc := &fakeECS{
		containerInstances: map[string]ecstypes.ContainerInstance{
			"ci-1": {ContainerInstanceArn: aws.String("ci-1"), Ec2InstanceId: aws.String("i-1")},
		},
	}
	svcec2 := &fakeEC2{instances: map[string]ec2types.Instance{"i-1": *ec2Instance("10.0.0.1")}}

	found := ecsTask("t1", "c1", "RUNNING", ecstypes.LaunchTypeEc2, aws.String("ci-1"))
	// DescribeContainerInstances reports a failure for ci-2; this used to
	// dereference a nil container instance.
	missing := ecsTask("t2", "c1", "RUNNING", ecstypes.LaunchTypeEc2, aws.String("ci-2"))
	tasks := []*AugmentedTask{{Task: &found}, {Task: &missing}}

	tasks, err := AddContainerInstancesToTasks(context.Background(), svc, svcec2, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].EC2Instance == nil || *tasks[0].EC2Instance.InstanceId != "i-1" {
		t.Errorf("task t1 instance = %v, want i-1", tasks[0].EC2Instance)
	}
	if tasks[1].EC2Instance != nil {
		t.Errorf("task t2 instance = %v, want nil", tasks[1].EC2Instance)
	}
}

func TestDescribeInstancesChunks(t *testing.T) {
	svcec2 := &fakeEC2{instances: map[string]ec2types.Instance{}}
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = "i-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		svcec2.instances[ids[i]] = ec2types.Instance{InstanceId: aws.String(ids[i])}
	}
	got, err := DescribeInstances(context.Background(), svcec2, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 250 {
		t.Errorf("got %d instances, want 250", len(got))
	}
	if len(svcec2.calls) != 3 {
		t.Errorf("made %d calls, want 3", len(svcec2.calls))
	}

	got, err = DescribeInstances(context.Background(), svcec2, nil)
	if err != nil || len(got) != 0 || len(svcec2.calls) != 3 {
		t.Errorf("empty input: got %v, %v after %d calls", got, err, len(svcec2.calls))
	}
}

func TestGetAugmentedTasksEndToEnd(t *testing.T) {
	td := ecstypes.TaskDefinition{
		TaskDefinitionArn:    aws.String("arn:td/app:1"),
		Family:               aws.String("app"),
		Revision:             1,
		ContainerDefinitions: []ecstypes.ContainerDefinition{containerDef("app", map[string]string{"PROMETHEUS_EXPORTER_PORT": "9100"})},
	}
	svc := &fakeECS{
		taskPages: map[string][][]string{"c1": {{"ec2", "fargate"}}},
		tasks: map[string]ecstypes.Task{
			"ec2":     ecsTask("ec2", "c1", "RUNNING", ecstypes.LaunchTypeEc2, aws.String("ci-1"), container("app", binding(9100, 32769))),
			"fargate": ecsTask("fargate", "c1", "RUNNING", ecstypes.LaunchTypeFargate, nil, awsvpcContainer("app", "10.1.0.5")),
		},
		taskDefinitions: map[string]ecstypes.TaskDefinition{"arn:td/app:1": td},
		containerInstances: map[string]ecstypes.ContainerInstance{
			"ci-1": {ContainerInstanceArn: aws.String("ci-1"), Ec2InstanceId: aws.String("i-1")},
		},
	}
	svcec2 := &fakeEC2{instances: map[string]ec2types.Instance{"i-1": *ec2Instance("10.0.0.1")}}

	tasks, err := GetAugmentedTasks(context.Background(), svc, svcec2, []string{"c1"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tsk := range tasks {
		got = append(got, targets(tsk.ExporterInformation(defaultConfig()))...)
	}
	sort.Strings(got)
	if want := []string{"10.0.0.1:32769", "10.1.0.5:9100"}; !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}
