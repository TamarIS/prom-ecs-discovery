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
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/go-yaml/yaml"
	"io/ioutil"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type labels struct {
	TaskArn       string
	TaskName      string
	JobName       string
	TaskRevision  string
	TaskGroup     string
	ClusterArn    string
	ContainerName string
	ContainerArn  string
	DockerImage   string
	MetricsPath   string
}

// Docker label for enabling dynamic port detection
const dynamicPortLabel = "PROMETHEUS_DYNAMIC_EXPORT"

var cluster = flag.String("config.cluster", "", "name of the cluster to scrape")
var clusterfilters = flag.String("config.cluster_filters", "", "commad delimited regex filters for clusters")
var region = flag.String("config.region", "eu-west-1", "name of the region to scrape")
var outFile = flag.String("config.write-to", "ecs_file_sd.yml", "path of file to write ECS service discovery information to")
var interval = flag.Duration("config.scrape-interval", 60*time.Second, "interval at which to scrape the AWS API for ECS service discovery information")
var times = flag.Int("config.scrape-times", 0, "how many times to scrape before exiting (0 = infinite)")
var roleArn = flag.String("config.role-arn", "", "ARN of the role to assume when scraping the AWS API (optional)")
var awsProfile = flag.String("config.aws-profile", "default", "name of the profile to use from the aws credentials file (optional)")
var prometheusPortLabel = flag.String("config.port-label", "PROMETHEUS_EXPORTER_PORT", "Docker label to define the scrape port of the application (if missing an application won't be scraped)")
var prometheusPathLabel = flag.String("config.path-label", "PROMETHEUS_EXPORTER_PATH", "Docker label to define the scrape path of the application")
var prometheusFilterLabel = flag.String("config.filter-label", "", "Docker label (and optionally value) to require to scrape the application")
var prometheusServerNameLabel = flag.String("config.server-name-label", "PROMETHEUS_EXPORTER_SERVER_NAME", "Docker label to define the server name")
var prometheusJobNameLabel = flag.String("config.job-name-label", "PROMETHEUS_EXPORTER_JOB_NAME", "Docker label to define the job name")
var prometheusDynamicPortDetection = flag.Bool("config.dynamic-port-detection", false, fmt.Sprintf("If true, only tasks with the Docker label %s=1 will be scraped", dynamicPortLabel))

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

// GetClusters retrieves a list of *ClusterArns from Amazon ECS,
// dealing with the mandatory pagination as needed.
func GetClusters(svc *ecs.Client) (*ecs.ListClustersOutput, error) {
	input := &ecs.ListClustersInput{}
	output := &ecs.ListClustersOutput{}
	// configure the custom retryer

	for {
		listClusters, err := svc.ListClusters(context.Background(), input)
		if err != nil {
			logError(err)
			return nil, err
		}

		output.ClusterArns = append(output.ClusterArns, listClusters.ClusterArns...)
		if listClusters.NextToken == nil {
			break
		}
		input.NextToken = listClusters.NextToken
	}
	return output, nil
}

// AugmentedTask represents an ECS task augmented with an extra set of
// structures representing the ECS task definition and EC2 instance
// associated with the running task.
type AugmentedTask struct {
	*ecsTypes.Task
	TaskDefinition     *ecsTypes.TaskDefinition
	TaskDefinitionTags []ecsTypes.Tag
	EC2Instance        *ec2Types.Instance
}

// PrometheusContainer represents a tuple of information
// (Container Name, Container ARN, Docker image, Port)
// extracted from a task, its task definition
type PrometheusContainer struct {
	ContainerName string
	ContainerArn  string
	DockerImage   string
	Port          int
}

// PrometheusTaskInfo is the final structure that will be
// output as a Prometheus file service discovery config.
type PrometheusTaskInfo struct {
	Targets  []string
	Labels   labels
	EC2Tags  map[string]string
	TaskTags map[string]string
}

// PrometheusTaskInfoOutput .
type PrometheusTaskInfoOutput struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}

// ExporterInformation returns a list of []*PrometheusTaskInfo
// enumerating the IPs, ports that the task's containers exports
// to Prometheus (one per container), so long as the Docker
// labels in its corresponding container definition for the
// container in the task has a PROMETHEUS_EXPORTER_PORT
//
// Example:
//     ...
//             "Name": "apache",
//             "DockerLabels": {
//                  "PROMETHEUS_EXPORTER_PORT": "1234"
//              },
//     ...
//              "PortMappings": [
//                {
//                  "ContainerPort": 1883,
//                  "HostPort": 0,
//                  "Protocol": "tcp"
//                },
//                {
//                  "ContainerPort": 1234,
//                  "HostPort": 0,
//                  "Protocol": "tcp"
//                }
//              ],
//     ...
func (t *AugmentedTask) ExporterInformation() []*PrometheusTaskInfo {
	ret := []*PrometheusTaskInfo{}
	ec2Tags := make(map[string]string)
	taskTags := make(map[string]string)
	var host string
	var ip string

	if t.LaunchType != ecsTypes.LaunchTypeFargate {
		if t.EC2Instance == nil {
			return ret
		}
		if len(t.EC2Instance.NetworkInterfaces) == 0 {
			return ret
		}

		for _, iface := range t.EC2Instance.NetworkInterfaces {
			if iface.PrivateIpAddress != nil && *iface.PrivateIpAddress != "" &&
				iface.PrivateDnsName != nil && *iface.PrivateDnsName != "" &&
				*iface.PrivateDnsName == *t.EC2Instance.PrivateDnsName {
				ip = *iface.PrivateIpAddress
				break
			}
		}
		if ip == "" {
			return ret
		}

	}

	var filter []string
	if *prometheusFilterLabel != "" {
		filter = strings.Split(*prometheusFilterLabel, "=")
	}

	for _, i := range t.Containers {
		// Let's go over the containers to see which ones are defined
		var d ecsTypes.ContainerDefinition
		for _, d = range t.TaskDefinition.ContainerDefinitions {
			if *i.Name == *d.Name {
				// Aha, the container definition match this container we
				// are inspecting, stop the loop cos we got the D now.
				break
			}
		}
		if *i.Name != *d.Name && t.LaunchType != ecsTypes.LaunchTypeFargate {
			// Nope, no match, this container cannot be exported.  We continue.
			continue
		}

		var hostPort int64
		if *prometheusDynamicPortDetection {
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

			if port := i.NetworkBindings[0].HostPort; port != nil {
				hostPort = int64(*port)
			}
		} else {
			v, ok := d.DockerLabels[*prometheusPortLabel]
			if !ok {
				// Nope, no Prometheus-exported port in this container def.
				// This container is no good.  We continue.
				continue
			}

			if len(filter) != 0 {
				v, ok := d.DockerLabels[filter[0]]
				if !ok {
					// Nope, the filter label isn't present.
					continue
				}
				if len(filter) == 2 && v != filter[1] {
					// Nope, the filter label value doesn't match.
					continue
				}
			}

			var exporterPort int
			var err error
			if exporterPort, err = strconv.Atoi(v); err != nil || exporterPort < 0 {
				// This container has an invalid port definition.
				// This container is no good.  We continue.
				continue
			}

			if len(i.NetworkBindings) > 0 {
				for _, nb := range i.NetworkBindings {
					if int(*nb.ContainerPort) == exporterPort {
						hostPort = int64(*nb.HostPort)
					}
				}
			} else {
				for _, ni := range i.NetworkInterfaces {
					if *ni.PrivateIpv4Address != "" {
						ip = *ni.PrivateIpv4Address
					}
				}
				hostPort = int64(exporterPort)
			}
		}

		var exporterServerName string
		var exporterPath string
		var ok bool
		exporterServerName, ok = d.DockerLabels[*prometheusServerNameLabel]
		if ok {
			host = strings.TrimRight(exporterServerName, "/")
		} else {
			// No server name, so fall back to ip address
			host = ip
		}

		labels := labels{
			TaskArn:       *t.TaskArn,
			TaskName:      *t.TaskDefinition.Family,
			JobName:       d.DockerLabels[*prometheusJobNameLabel],
			TaskRevision:  fmt.Sprintf("%d", t.TaskDefinition.Revision),
			TaskGroup:     *t.Group,
			ClusterArn:    *t.ClusterArn,
			ContainerName: *i.Name,
			ContainerArn:  *i.ContainerArn,
			DockerImage:   *d.Image,
		}

		exporterPath, ok = d.DockerLabels[*prometheusPathLabel]
		if ok {
			labels.MetricsPath = exporterPath
		}

		for _, tag := range t.EC2Instance.Tags {
			ec2Tags[cleanTagKeyForPrometheus(*tag.Key)] = *tag.Value
		}

		for _, tag := range t.TaskDefinitionTags {
			taskTags[cleanTagKeyForPrometheus(*tag.Key)] = *tag.Value
		}
		for _, tag := range t.Tags {
			taskTags[cleanTagKeyForPrometheus(*tag.Key)] = *tag.Value
		}

		ret = append(ret, &PrometheusTaskInfo{
			Targets:  []string{fmt.Sprintf("%s:%d", host, hostPort)},
			Labels:   labels,
			EC2Tags:  ec2Tags,
			TaskTags: taskTags,
		})
	}
	return ret
}

func cleanTagKeyForPrometheus(tagKey string) string {
	r, err := regexp.Compile("[:-]")
	if err != nil {
		log.Fatalf("Error compiling regexp: %+v\n", err)
	}

	return r.ReplaceAllLiteralString(strings.ToLower(tagKey), "_")
}

// AddTaskDefinitionsOfTasks adds to each Task the TaskDefinition
// corresponding to it.
func AddTaskDefinitionsOfTasks(svc *ecs.Client, taskList []*AugmentedTask) ([]*AugmentedTask, error) {
	task2def := make(map[string]*ecsTypes.TaskDefinition)
	task2tags := make(map[string][]ecsTypes.Tag)
	for _, task := range taskList {
		task2def[*task.TaskDefinitionArn] = nil
		task2tags[*task.TaskDefinitionArn] = nil
	}

	jobs := make(chan *ecs.DescribeTaskDefinitionInput, len(task2def))
	results := make(chan struct {
		out *ecs.DescribeTaskDefinitionOutput
		err error
	}, len(task2def))

	for w := 1; w <= 4; w++ {
		go func() {
			for in := range jobs {
				td, err := svc.DescribeTaskDefinition(context.Background(), in)
				results <- struct {
					out *ecs.DescribeTaskDefinitionOutput
					err error
				}{td, err}
			}
		}()
	}

	for tn := range task2def {
		m := string(append([]byte{}, tn...))
		jobs <- &ecs.DescribeTaskDefinitionInput{TaskDefinition: &m, Include: []ecsTypes.TaskDefinitionField{ecsTypes.TaskDefinitionFieldTags}}
	}
	close(jobs)

	var err error
	for range task2def {
		result := <-results
		if result.err != nil {
			err = result.err
			log.Printf("Error describing task definition: %s", err)
		} else {
			task2def[*result.out.TaskDefinition.TaskDefinitionArn] = result.out.TaskDefinition
			task2tags[*result.out.TaskDefinition.TaskDefinitionArn] = result.out.Tags
		}
	}
	if err != nil {
		return nil, err
	}

	for _, task := range taskList {
		task.TaskDefinition = task2def[*task.TaskDefinitionArn]
		task.TaskDefinitionTags = task2tags[*task.TaskDefinitionArn]
	}
	return taskList, nil
}

// StringToStarString converts a list of strings to a list of
// pointers to strings, which is a common requirement of the
// Amazon API.
func StringToStarString(s []string) []*string {
	c := make([]*string, 0, len(s))
	for n := range s {
		c = append(c, &s[n])
	}
	return c
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

// DescribeInstancesUnpaginated describes a list of EC2 instances.
// It is unpaginated because the API function does not require
// pagination.
func DescribeInstancesUnpaginated(svc *ec2.Client, instanceIds []string) ([]ec2Types.Instance, error) {
	if len(instanceIds) == 0 {
		return nil, nil
	}
	finalOutput := &ec2.DescribeInstancesOutput{}
	splittedInstanceIds := SplitArray(instanceIds, 100)
	for _, chunkedInstanceIds := range splittedInstanceIds {
		input := &ec2.DescribeInstancesInput{
			InstanceIds: chunkedInstanceIds,
		}
		for {

			output, err := svc.DescribeInstances(context.Background(), input)
			if err != nil {
				return nil, err
			}
			log.Printf("Described %d EC2 reservations", len(output.Reservations))
			finalOutput.Reservations = append(finalOutput.Reservations, output.Reservations...)
			if output.NextToken == nil {
				break
			}
			input.NextToken = output.NextToken
		}
	}
	result := []ec2Types.Instance{}
	for _, rsv := range finalOutput.Reservations {
		for _, i := range rsv.Instances {
			result = append(result, i)
		}
	}
	return result, nil
}

// AddContainerInstancesToTasks adds to each Task the EC2 instance
// running its containers.
func AddContainerInstancesToTasks(svc *ecs.Client, svcec2 *ec2.Client, taskList []*AugmentedTask) ([]*AugmentedTask, error) {

	clusterArnToContainerInstancesArns := make(map[string]map[string]*ecsTypes.ContainerInstance)
	for _, task := range taskList {
		if task.ContainerInstanceArn != nil {
			if _, ok := clusterArnToContainerInstancesArns[*task.ClusterArn]; !ok {
				clusterArnToContainerInstancesArns[*task.ClusterArn] = make(map[string]*ecsTypes.ContainerInstance)
			}
			clusterArnToContainerInstancesArns[*task.ClusterArn][*task.ContainerInstanceArn] = nil
		}
	}

	instanceIDToEC2Instance := make(map[string]*ec2Types.Instance)
	for clusterArn, containerInstancesArns := range clusterArnToContainerInstancesArns {
		keys := make([]string, 0, len(containerInstancesArns))
		for k := range containerInstancesArns {
			keys = append(keys, k)
		}

		splittedKeys := SplitArray(keys, 100)
		for _, chunkedKeys := range splittedKeys {
			input := &ecs.DescribeContainerInstancesInput{
				Cluster:            &clusterArn,
				ContainerInstances: chunkedKeys,
			}

			output, err := svc.DescribeContainerInstances(context.Background(), input)
			if err != nil {
				return nil, err
			}

			if len(output.Failures) > 0 {
				log.Printf("Described %d failures in cluster %s", len(output.Failures), clusterArn)
			}
			for _, ci := range output.ContainerInstances {
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

	instances, err := DescribeInstancesUnpaginated(svcec2, keys)
	if err != nil {
		return nil, err
	}

	for _, i := range instances {
		inst := i
		instanceIDToEC2Instance[*i.InstanceId] = &inst
	}

	for _, task := range taskList {
		if task.ContainerInstanceArn != nil {
			containerInstance, ok := clusterArnToContainerInstancesArns[*task.ClusterArn][*task.ContainerInstanceArn]
			if !ok {
				log.Printf("Cannot find container instance %s in cluster %s", *task.ContainerInstanceArn, *task.ClusterArn)
				continue
			}
			instance, ok := instanceIDToEC2Instance[*containerInstance.Ec2InstanceId]
			if !ok {
				log.Printf("Cannot find EC2 instance %s", *containerInstance.Ec2InstanceId)
				continue
			}
			task.EC2Instance = instance
		}
	}

	return taskList, nil
}

// GetTasksOfClusters returns the EC2 tasks running in a list of Clusters.
func GetTasksOfClusters(svc *ecs.Client, svcec2 *ec2.Client, clusterArns []*string) ([]ecsTypes.Task, error) {
	jobs := make(chan *string, len(clusterArns))
	results := make(chan struct {
		out *ecs.DescribeTasksOutput
		err error
	}, len(clusterArns))

	for w := 1; w <= 4; w++ {
		go func() {
			for clusterArn := range jobs {
				input := &ecs.ListTasksInput{
					Cluster: clusterArn,
				}
				finalOutput := &ecs.DescribeTasksOutput{}
				var err error
				for {
					output, err1 := svc.ListTasks(context.Background(), input)
					if err1 != nil {
						err = err1
						log.Printf("Error listing tasks of cluster %s: %s", *clusterArn, err)
						break
					}
					if len(output.TaskArns) == 0 {
						break
					}
					log.Printf("Inspected cluster %s, found %d tasks", *clusterArn, len(output.TaskArns))
					descOutput, err2 := svc.DescribeTasks(context.Background(), &ecs.DescribeTasksInput{
						Cluster: clusterArn,
						Tasks:   output.TaskArns,
					})
					if err2 != nil {
						err = err2
						log.Printf("Error describing tasks of cluster %s: %s", *clusterArn, err)
						break
					}
					log.Printf("Described %d tasks in cluster %s", len(descOutput.Tasks), *clusterArn)
					if len(descOutput.Failures) > 0 {
						log.Printf("Described %d failures in cluster %s", len(descOutput.Failures), *clusterArn)
					}
					finalOutput.Tasks = append(finalOutput.Tasks, descOutput.Tasks...)
					finalOutput.Failures = append(finalOutput.Failures, descOutput.Failures...)
					if output.NextToken == nil {
						break
					}
					input.NextToken = output.NextToken
				}
				results <- struct {
					out *ecs.DescribeTasksOutput
					err error
				}{finalOutput, err}
			}
		}()
	}

	for _, clusterArn := range clusterArns {
		jobs <- clusterArn
	}
	close(jobs)

	tasks := []ecsTypes.Task{}
	for range clusterArns {
		result := <-results
		if result.err != nil {
			return nil, result.err
		}
		for _, task := range result.out.Tasks {
			tasks = append(tasks, task)
		}
	}

	return tasks, nil
}

// GetAugmentedTasks gets the fully AugmentedTasks running on
// a list of Clusters.
func GetAugmentedTasks(svc *ecs.Client, svcec2 *ec2.Client, clusterArns []*string) ([]*AugmentedTask, error) {
	simpleTasks, err := GetTasksOfClusters(svc, svcec2, clusterArns)
	if err != nil {
		return nil, err
	}

	tasks := []*AugmentedTask{}
	for i := 0; i < len(simpleTasks); i++ {
		tasks = append(tasks, &AugmentedTask{&simpleTasks[i], nil, nil, nil})
	}
	tasks, err = AddTaskDefinitionsOfTasks(svc, tasks)
	if err != nil {
		return nil, err
	}

	tasks, err = AddContainerInstancesToTasks(svc, svcec2, tasks)
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

func isMatchingAnyFilter(filters []*regexp.Regexp, arn string) bool {
	for _, reg := range filters {
		if reg.MatchString(arn) {
			return true
		}
	}
	return false
}

func compileFilters(filters []string) ([]*regexp.Regexp, error) {
	compiledFilters := make([]*regexp.Regexp, len(filters))
	for i, filter := range filters {
		f2chk, err := regexp.Compile("^[^/]*cluster/" + filter)
		if err != nil {
			return nil, err
		}
		compiledFilters[i] = f2chk
	}

	return compiledFilters, nil
}

func makePrometheusTaskInfoOutput(infos []*PrometheusTaskInfo) []*PrometheusTaskInfoOutput {
	outInfos := make([]*PrometheusTaskInfoOutput, len(infos))

	for i, info := range infos {
		labels := make(map[string]string)

		for k, v := range info.EC2Tags {
			labels["__meta_ec2_tag_"+k] = v
		}
		for k, v := range info.TaskTags {
			labels["__meta_task_tag_"+k] = v
		}
		labels["task_arn"] = info.Labels.TaskArn
		labels["task_name"] = info.Labels.TaskName
		if len(info.Labels.JobName) > 0 {
			labels["job"] = info.Labels.JobName
		}
		labels["task_revision"] = info.Labels.TaskRevision
		labels["task_group"] = info.Labels.TaskGroup
		labels["task_arn"] = info.Labels.TaskArn
		labels["cluster_arn"] = info.Labels.ClusterArn
		labels["container_name"] = info.Labels.ContainerName
		labels["container_arn"] = info.Labels.ContainerArn
		labels["docker_image"] = info.Labels.DockerImage
		if len(info.Labels.MetricsPath) > 0 {
			labels["__metrics_path__"] = info.Labels.MetricsPath
		}

		outInfos[i] = &PrometheusTaskInfoOutput{
			Targets: info.Targets,
			Labels:  labels,
		}
	}

	return outInfos
}

// STSAssumeRoleAPI defines the interface for the AssumeRole function.
// We use this interface to test the function using a mocked service.
type STSAssumeRoleAPI interface {
	AssumeRole(ctx context.Context,
		params *sts.AssumeRoleInput,
		optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// TakeRole gets temporary security credentials to access resources.
// Inputs:
//     c is the context of the method call, which includes the AWS Region.
//     api is the interface that defines the method call.
//     input defines the input arguments to the service call.
// Output:
//     If successful, an AssumeRoleOutput object containing the result of the service call and nil.
//     Otherwise, nil and an error from the call to AssumeRole.
func TakeRole(c context.Context, api STSAssumeRoleAPI, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	return api.AssumeRole(c, input)
}

func main() {
	flag.Parse()
	ctx := context.TODO()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region), config.WithSharedConfigProfile(*awsProfile))

	if err != nil {
		log.Fatal(err)
	}

	// Create service client value configured for credentials from assumed role.
	//Note: Not checking NextToken since test results are small dataset and do not need pagination.

	if *roleArn != "" {
		client := sts.NewFromConfig(cfg)
		provider := stscreds.NewAssumeRoleProvider(client, *roleArn)
		cfg.Credentials = aws.NewCredentialsCache(provider)
	}

	if len(*cluster) > 0 && len(*clusterfilters) > 0 {
		log.Fatalf("You Cannot pass cluster and cluster filter at same time.")
	}
	// Initialise AWS Service clients
	svc := ecs.NewFromConfig(cfg)
	//svc := ecs.New(sess)
	svcec2 := ec2.NewFromConfig(cfg)
	//svcec2 := ec2.New(sess)

	work := func() {
		var clusters *ecs.ListClustersOutput
		var clustersList []string

		if *cluster != "" {
			clustersList = append(clustersList, *cluster)
			descClustOut, err := svc.DescribeClusters(context.TODO(), &ecs.DescribeClustersInput{
				Clusters: clustersList,
				Include:  nil,
			})

			if err != nil {
				logError(err)
				return
			}

			if len(descClustOut.Clusters) == 0 {
				logError(fmt.Errorf("cluster not found %s", cluster))
				return
			}

			clusters = &ecs.ListClustersOutput{
				ClusterArns: []string{*descClustOut.Clusters[0].ClusterArn},
			}
		} else if *clusterfilters != "" {
			var filtered []string
			var cfilters = strings.Split(*clusterfilters, ",")
			fmt.Println("Processing Filters", cfilters)
			listClusterOut, err := GetClusters(svc)
			if err != nil {
				logError(err)
				return
			}

			filters2Check, err := compileFilters(cfilters)
			if err != nil {
				logError(err)
				return
			}

			for _, cArn := range listClusterOut.ClusterArns {
				if isMatchingAnyFilter(filters2Check, cArn) {
					filtered = append(filtered, cArn)
				}
			}
			log.Printf("Filtered clusters %s.", filtered)

			clusters = &ecs.ListClustersOutput{
				ClusterArns: filtered,
			}
		} else {
			listClusterOut, err := GetClusters(svc)
			if err != nil {
				logError(err)
				return
			}
			clusters = listClusterOut
		}

		tasks, err := GetAugmentedTasks(svc, svcec2, StringToStarString(clusters.ClusterArns))
		if err != nil {
			logError(err)
			return
		}
		infos := []*PrometheusTaskInfo{}
		for _, t := range tasks {
			info := t.ExporterInformation()
			infos = append(infos, info...)
		}
		m, err := yaml.Marshal(makePrometheusTaskInfoOutput(infos))
		if err != nil {
			logError(err)
			return
		}
		log.Printf("Writing %d discovered exporters to %s", len(infos), *outFile)
		err = ioutil.WriteFile(*outFile, m, 0644)
		if err != nil {
			logError(err)
			return
		}
	}
	s := time.NewTimer(1 * time.Millisecond)
	t := time.NewTicker(*interval)
	n := *times
	for {
		select {
		case <-s.C:
		case <-t.C:
		}
		work()
		n = n - 1
		if *times > 0 && n == 0 {
			break
		}
	}
}
