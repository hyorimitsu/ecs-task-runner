package etr

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	cw "github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/docker/distribution/reference"
	"github.com/google/uuid"
	"github.com/pottava/ecs-task-runner/lib"
	"github.com/pottava/ecs-task-runner/log"
	"golang.org/x/sync/errgroup"
)

var (
	requestID     string
	logGroup      string
	taskDefARN    *string
	exitNormally  *int64
	exitWithError *int64
)

func init() {
	requestID = fmt.Sprintf("ecs-task-runner-%s", uuid.New().String())
	logGroup = fmt.Sprintf("/ecs/%s", requestID)
	exitNormally = aws.Int64(0)
	exitWithError = aws.Int64(1)
}

// Run runs the docker image on Amazon ECS
func Run(ctx context.Context, conf *RunConfig) (exitCode *int64, err error) {
	startedAt := time.Now()

	if conf.Common.IsDebugMode {
		log.PrintJSON(conf)
	}
	// Check AWS credentials
	sess, err := lib.Session(conf.Aws.AccessKey, conf.Aws.SecretKey, conf.Aws.Region, nil)
	if err != nil {
		return exitWithError, err
	}
	eg, _ := errgroup.WithContext(context.Background())

	// Check existence of the image on ECR
	var image *string
	eg.Go(func() (err error) {
		image, err = validateImageName(ctx, conf, sess)
		return err
	})
	// Ensure resource existence
	eg.Go(func() (err error) {
		return ensureAWSResources(ctx, sess, conf)
	})
	if err = eg.Wait(); err != nil {
		return exitWithError, err
	}
	// Create AWS resources
	var taskDefInput *ecs.RegisterTaskDefinitionInput
	taskDefInput, err = createResouces(ctx, sess, conf, image)
	if err != nil {
		DeleteResouces(conf.Aws, conf.Common)
		return exitWithError, err
	}
	// Run the ECS task
	runTaskAt := time.Now()
	tasks, runconfig, err := run(ctx, sess, conf)
	if err != nil {
		DeleteResouces(conf.Aws, conf.Common)
		return exitWithError, err
	}
	// Asynchronous job
	if aws.BoolValue(conf.Asynchronous) {
		// Wait for its start
		tasks, err = waitForTask(ctx, sess, conf.Common, tasks, func(task *ecs.Task) bool {
			return !strings.EqualFold(aws.StringValue(task.LastStatus), "PROVISIONING") &&
				!strings.EqualFold(aws.StringValue(task.LastStatus), "PENDING")
		})
		if err != nil {
			DeleteResouces(conf.Aws, conf.Common)
			return exitWithError, err
		}
		outputRunResults(ctx, conf, startedAt, runTaskAt, nil, nil, taskDefInput, runconfig, tasks)
		deleteResoucesImmediately(conf.Aws, conf.Common)

		if len(tasks) == 0 || len(tasks[0].Containers) == 0 {
			return exitWithError, nil
		}
		return exitNormally, nil
	}
	// Wait for its done
	tasks, err = waitForTask(ctx, sess, conf.Common, tasks, func(task *ecs.Task) bool {
		// dont have to wait until its 'stopped'
		// return strings.EqualFold(aws.StringValue(task.LastStatus), "STOPPED")
		return task.ExecutionStoppedAt != nil
	})
	if err != nil {
		DeleteResouces(conf.Aws, conf.Common)
		return exitWithError, err
	}
	// Retrieve app log
	logs := lib.RetrieveLogs(ctx, sess, tasks, logGroup, logPrefix)
	retrieveLogsAt := time.Now()

	// Delete AWS resources
	DeleteResouces(conf.Aws, conf.Common)

	// Format the result
	outputRunResults(ctx, conf, startedAt, runTaskAt, &retrieveLogsAt, logs, taskDefInput, runconfig, tasks)

	if len(tasks) == 0 || len(tasks[0].Containers) == 0 {
		return exitWithError, nil
	}
	exitCode = exitNormally
	for _, task := range tasks {
		for _, container := range task.Containers {
			if aws.Int64Value(exitCode) != 0 {
				break
			}
			exitCode = container.ExitCode
		}
	}
	return exitCode, nil
}

// Stop stops the Fargate container on Amazon ECS
func Stop(ctx context.Context, conf *StopConfig) (exitCode *int64, err error) {

	// Check AWS credentials
	sess, err := lib.Session(conf.Aws.AccessKey, conf.Aws.SecretKey, conf.Aws.Region, nil)
	if err != nil {
		return exitWithError, err
	}
	// Ensure parameters
	requestID = aws.StringValue(conf.RequestID)
	if isEmpty(conf.Common.EcsCluster) {
		conf.Common.EcsCluster = conf.RequestID
	}
	if conf.Common.IsDebugMode {
		log.PrintJSON(conf)
	}
	// Retrieve all tasks to check cluster can be deleted or not
	all, err := ecs.New(sess).ListTasksWithContext(ctx, &ecs.ListTasksInput{
		Cluster: conf.Common.EcsCluster,
	})
	if err != nil {
		return exitWithError, err
	}
	// Stop the task
	tasks := []*ecs.Task{}
	if len(conf.TaskARNs) == 0 {
		conf.TaskARNs = all.TaskArns
	}
	for _, taskARN := range conf.TaskARNs {
		task, err := lib.StopTask(ctx, sess, conf.Common.EcsCluster, taskARN)
		if err != nil {
			return exitWithError, err
		}
		tasks = append(tasks, task)
	}
	tasks, _ = waitForTask(ctx, sess, conf.Common, tasks, func(task *ecs.Task) bool { // nolint
		return task.ExecutionStoppedAt != nil
	})
	logs := lib.RetrieveLogs(ctx, sess, tasks, logGroup, logPrefix)
	outputStopResults(ctx, conf, logs, tasks)

	// Delete AWS resources
	if len(all.TaskArns) == len(tasks) {
		deleteResoucesInTheEnd(conf.Aws, conf.Common)
	}
	return exitNormally, nil
}

func isEmpty(candidate *string) bool {
	return candidate == nil || aws.StringValue(candidate) == ""
}

func validateImageName(ctx context.Context, conf *RunConfig, sess *session.Session) (*string, error) {
	imageHost, imageName, imageTag, err := parseImageName(conf.Image)
	if err != nil {
		log.Errors.Println("Provided image name is invalid.")
		return nil, err
	}
	// Try to make up ECR image name
	if aws.BoolValue(conf.ForceECR) {
		account, err := sts.New(sess).GetCallerIdentityWithContext(ctx, nil)
		if err != nil {
			return nil, errors.New("Provided AWS credentials are invalid")
		}
		if conf.Common.IsDebugMode {
			log.PrintJSON(account)
		}
		conf.Aws.accountID = aws.StringValue(account.Account)
		if !strings.Contains(aws.StringValue(imageHost), conf.Aws.accountID) {
			imageName = aws.String(fmt.Sprintf(
				"%s/%s",
				aws.StringValue(imageHost),
				aws.StringValue(imageName),
			))
			imageHost = aws.String(fmt.Sprintf(
				"%s.dkr.ecr.%s.amazonaws.com",
				account,
				aws.StringValue(conf.Aws.Region),
			))
		}
	}
	if strings.Contains(aws.StringValue(imageHost), "amazonaws.com") {
		if _, err := ecr.New(sess).DescribeRepositoriesWithContext(ctx, &ecr.DescribeRepositoriesInput{
			RepositoryNames: []*string{imageName},
		}); err != nil {
			return nil, err
		}
	}
	if aws.StringValue(imageHost) == "" {
		return aws.String(fmt.Sprintf(
			"%s%s",
			aws.StringValue(imageName),
			aws.StringValue(imageTag),
		)), nil
	}
	return aws.String(fmt.Sprintf(
		"%s/%s%s",
		aws.StringValue(imageHost),
		aws.StringValue(imageName),
		aws.StringValue(imageTag),
	)), nil
}

func parseImageName(value string) (*string, *string, *string, error) {
	ref, err := reference.Parse(value)
	if err != nil {
		return nil, nil, nil, err
	}
	imageHost := ""
	imageName := ""
	if candidate, ok := ref.(reference.Named); ok {
		imageHost, imageName = reference.SplitHostname(candidate)
	}
	imageTag := ":latest"
	if candidate, ok := ref.(reference.Tagged); ok {
		imageTag = ":" + candidate.Tag()
	}
	if candidate, ok := ref.(reference.Digested); ok {
		digest := candidate.Digest()
		if digest.Validate() == nil {
			imageTag = "@" + digest.String()
		}
	}
	return aws.String(imageHost), aws.String(imageName), aws.String(imageTag), nil
}

func ensureAWSResources(ctx context.Context, sess *session.Session, conf *RunConfig) error {
	eg, _ := errgroup.WithContext(context.Background())
	vpc := lib.FindDefaultVPC(ctx, sess)

	// Ensure cluster existence
	eg.Go(func() error {
		if isEmpty(conf.Common.EcsCluster) {
			conf.Common.EcsCluster = aws.String(requestID)
		}
		return lib.CreateClusterIfNotExist(ctx, sess, conf.Common.EcsCluster)
	})

	// Ensure subnets existence
	eg.Go(func() (err error) {
		subnets := []*string{}
		if conf.Subnets == nil || len(conf.Subnets) == 0 {
			defSubnet := lib.FindDefaultSubnet(ctx, sess, vpc)
			if defSubnet == nil {
				return errors.New("There is no default subnet")
			}
			subnets = append(subnets, defSubnet)
		} else {
			for _, arg := range conf.Subnets {
				for _, subnet := range strings.Split(aws.StringValue(arg), ",") {
					subnets = append(subnets, aws.String(subnet))
				}
			}
		}
		conf.Subnets = subnets
		return nil
	})

	// Ensure security-group existence
	eg.Go(func() (err error) {
		sgs := []*string{}
		if conf.SecurityGroups == nil || len(conf.SecurityGroups) == 0 {
			defSecurityGroup := lib.FindDefaultSecurityGroup(ctx, sess, vpc)
			if defSecurityGroup == nil {
				return errors.New("There is no default security group")
			}
			sgs = append(sgs, defSecurityGroup)
		} else {
			for _, arg := range conf.SecurityGroups {
				for _, sg := range strings.Split(aws.StringValue(arg), ",") {
					sgs = append(sgs, aws.String(sg))
				}
			}
		}
		conf.SecurityGroups = sgs
		return nil
	})
	return eg.Wait()
}

const (
	ecsManagedExecPolicyArn      = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
	ecsManagedExecPolicyDocument = `{
  "Statement": [{
    "Effect": "Allow",
    "Action": "sts:AssumeRole",
    "Principal": {
      "Service": "ecs-tasks.amazonaws.com"
    }
  }]
}`
	kmsCustomKeyID               = "\"arn:aws:kms:%s:%s:key:%s\","
	ecsPrivateRepoPolicyDocument = `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "kms:Decrypt",
      "secretsmanager:GetSecretValue"
    ],
    "Resource": [
      %s
      "%s"
    ]
  }]
}`
	fargate   = "FARGATE"
	logPrefix = "fargate"
	awsVPC    = "awsvpc"
	awsCWLogs = "awslogs"
)

var (
	dockerCreds *string
	credsPolicy *string
)

func createResouces(ctx context.Context, sess *session.Session, conf *RunConfig, image *string) (taskDefInput *ecs.RegisterTaskDefinitionInput, e error) {
	eg, _ := errgroup.WithContext(context.Background())

	eg.Go(func() error {
		// Make a temporary log group
		return lib.CreateLogGroup(ctx, sess, logGroup)
	})
	eg.Go(func() (err error) {
		if !isEmpty(conf.DockerUser) {
			// Store private registry credentials in AWS SecretsManager
			dockerCreds, err = lib.CreateSecret(
				ctx, sess,
				aws.String(requestID),
				conf.KMSCustomKeyID,
				aws.String(fmt.Sprintf(
					`{"username":"%s","password":"%s"}`,
					aws.StringValue(conf.DockerUser),
					aws.StringValue(conf.DockerPassword),
				)),
			)
			if err != nil {
				return err
			}
		}
		// Make a temporary IAM role
		var execRoleArn *string
		execRoleArn, err = createIAMRole(ctx, sess, conf)
		if err != nil {
			return err
		}
		// Make a temporary task definition
		taskDefARN, taskDefInput, err = registerTaskDef(ctx, sess, conf, image, execRoleArn)
		return
	})
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return taskDefInput, nil
}

func createIAMRole(ctx context.Context, sess *session.Session, conf *RunConfig) (*string, error) {
	roleName := conf.Common.ExecRoleName
	role, err := iam.New(sess).GetRoleWithContext(ctx, &iam.GetRoleInput{
		RoleName: roleName,
	})
	var execRoleArn *string
	if err == nil && role.Role != nil {
		execRoleArn = role.Role.Arn
	} else {
		out, e := iam.New(sess).CreateRoleWithContext(ctx, &iam.CreateRoleInput{
			RoleName:                 roleName,
			AssumeRolePolicyDocument: aws.String(ecsManagedExecPolicyDocument),
			Path:                     aws.String("/"),
		})
		if e != nil {
			return nil, e
		}
		execRoleArn = out.Role.Arn
	}
	if err = lib.AttachPolicy(ctx, sess, roleName, aws.String(ecsManagedExecPolicyArn)); err != nil {
		return nil, err
	}
	// If you'd like to use private repo, the execution role has to have a special policy.
	// https://docs.aws.amazon.com/AmazonECS/latest/developerguide/private-auth.html
	if !isEmpty(conf.DockerUser) && dockerCreds != nil {
		keyResource := ""
		if !isEmpty(conf.KMSCustomKeyID) {
			keyResource = fmt.Sprintf(
				kmsCustomKeyID,
				aws.StringValue(conf.Aws.Region),
				conf.Aws.accountID,
				aws.StringValue(conf.KMSCustomKeyID),
			)
		}
		policy, err := lib.CreatePolicy(
			ctx, sess,
			fmt.Sprintf("ecs-custom-%s", requestID),
			fmt.Sprintf(
				ecsPrivateRepoPolicyDocument,
				keyResource,
				aws.StringValue(dockerCreds)))
		if err != nil {
			return nil, err
		}
		credsPolicy = policy.Arn
		if err = lib.AttachPolicy(ctx, sess, roleName, credsPolicy); err != nil {
			return nil, err
		}
	}
	return execRoleArn, nil
}

func registerTaskDef(ctx context.Context, sess *session.Session, conf *RunConfig, image, execRoleArn *string) (*string, *ecs.RegisterTaskDefinitionInput, error) {
	environments := []*ecs.KeyValuePair{}
	for key, val := range conf.Environments {
		environments = append(environments, &ecs.KeyValuePair{
			Name:  aws.String(key),
			Value: val,
		})
	}
	ports := []*ecs.PortMapping{}
	for _, port := range conf.Ports {
		ports = append(ports, &ecs.PortMapping{
			ContainerPort: port,
		})
	}
	input := ecs.RegisterTaskDefinitionInput{
		Family:                  conf.TaskDefFamily,
		RequiresCompatibilities: []*string{aws.String(fargate)},
		ExecutionRoleArn:        execRoleArn,
		TaskRoleArn:             conf.TaskRoleArn,
		Cpu:                     conf.CPU,
		Memory:                  conf.Memory,
		NetworkMode:             aws.String(awsVPC),
		ContainerDefinitions: []*ecs.ContainerDefinition{
			&ecs.ContainerDefinition{
				Name:         aws.String("app"),
				Image:        image,
				EntryPoint:   conf.Entrypoint,
				Command:      conf.Commands,
				Environment:  environments,
				PortMappings: ports,
				DockerLabels: conf.Labels,
				Essential:    aws.Bool(true),
				LogConfiguration: &ecs.LogConfiguration{
					LogDriver: aws.String(awsCWLogs),
					Options: map[string]*string{
						"awslogs-region":        conf.Aws.Region,
						"awslogs-group":         aws.String(logGroup),
						"awslogs-stream-prefix": aws.String(logPrefix),
					},
				},
			},
		},
	}
	// If you'd like to use private repo, RepositoryCredentials should be specified.
	// https://docs.aws.amazon.com/AmazonECS/latest/developerguide/private-auth.html
	if !isEmpty(conf.DockerUser) && len(input.ContainerDefinitions) > 0 && dockerCreds != nil {
		input.ContainerDefinitions[0].RepositoryCredentials = &ecs.RepositoryCredentials{
			CredentialsParameter: dockerCreds,
		}
	}
	if conf.Common.IsDebugMode {
		log.PrintJSON(input)
	}
	out, err := ecs.New(sess).RegisterTaskDefinitionWithContext(ctx, &input)
	if err != nil {
		return nil, nil, err
	}
	return out.TaskDefinition.TaskDefinitionArn, &input, nil
}

func run(ctx context.Context, sess *session.Session, conf *RunConfig) ([]*ecs.Task, *ecs.RunTaskInput, error) {
	assignPublicIP := "ENABLED"
	if !aws.BoolValue(conf.AssignPublicIP) {
		assignPublicIP = "DISABLED"
	}
	input := ecs.RunTaskInput{
		Cluster:        conf.Common.EcsCluster,
		LaunchType:     aws.String(fargate),
		TaskDefinition: taskDefARN,
		Count:          conf.NumberOfTasks,
		NetworkConfiguration: &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				AssignPublicIp: aws.String(assignPublicIP),
				Subnets:        conf.Subnets,
				SecurityGroups: conf.SecurityGroups,
			},
		},
	}
	if conf.Common.IsDebugMode {
		log.PrintJSON(input)
	}
	// Avoid the following error
	// ClientException: ECS was unable to assume the role that was provided for this task.
	timeout := time.After(30 * time.Second)
	for {
		var err error
		select {
		case <-timeout:
			return nil, nil, errors.New("The execute role for this task was not in Active in 30sec")
		default:
			var out *ecs.RunTaskOutput
			out, err = ecs.New(sess).RunTaskWithContext(ctx, &input)
			if err == nil {
				return out.Tasks, &input, nil
			}
			if ae, ok := err.(awserr.Error); ok && strings.EqualFold(ae.Code(), ecs.ErrCodeClientException) {
				time.Sleep(1 * time.Second)
				continue
			}
			return nil, nil, err
		}
	}
}

type judgeFunc func(task *ecs.Task) bool

func waitForTask(ctx context.Context, sess *session.Session, conf *CommonConfig, tasks []*ecs.Task, judge judgeFunc) ([]*ecs.Task, error) {
	timeout := time.After(time.Duration(aws.Int64Value(conf.Timeout)) * time.Minute)
	taskARNs := []*string{}
	for _, task := range tasks {
		taskARNs = append(taskARNs, task.TaskArn)
	}
	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("The task did not finish in %d minutes", aws.Int64Value(conf.Timeout))
		default:
			tasks, err := ecs.New(sess).DescribeTasksWithContext(ctx, &ecs.DescribeTasksInput{
				Cluster: conf.EcsCluster,
				Tasks:   taskARNs,
			})
			if err != nil {
				return nil, err
			}
			if len(tasks.Tasks) > 0 {
				done := true
				for _, task := range tasks.Tasks {
					done = done && judge(task)
				}
				if done {
					if conf.IsDebugMode {
						log.PrintJSON(tasks.Tasks)
					}
					return tasks.Tasks, nil
				}
			}
			time.Sleep(1 * time.Second)
		}
	}
}

// DeleteResouces deletes temporary AWS resources
func DeleteResouces(aws *AwsConfig, conf *CommonConfig) {
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		deleteResoucesImmediately(aws, conf)
	}()
	go func() {
		defer wg.Done()
		deleteResoucesInTheEnd(aws, conf)
	}()
	wg.Wait()
}

func deleteResoucesImmediately(aws *AwsConfig, conf *CommonConfig) {
	sess, err := lib.Session(aws.AccessKey, aws.SecretKey, aws.Region, nil)
	if err != nil {
		return
	}
	wg := sync.WaitGroup{}
	wg.Add(3)

	// Delete the private registry creds in Secrets Manager
	go func() {
		defer wg.Done()
		lib.DeleteSecret(sess, dockerCreds, true, conf.IsDebugMode)
	}()
	// Delete the IAM policy for private registry creds
	go func() {
		defer wg.Done()
		if dockerCreds != nil {
			lib.DeletePolicy(sess, conf.ExecRoleName, credsPolicy, conf.IsDebugMode)
		}
	}()
	// Delete the temporary task definition
	go func() {
		defer wg.Done()
		lib.DeregisterTaskDef(sess, taskDefARN, conf.IsDebugMode)
	}()
	wg.Wait()
}

func deleteResoucesInTheEnd(aws *AwsConfig, conf *CommonConfig) {
	sess, err := lib.Session(aws.AccessKey, aws.SecretKey, aws.Region, nil)
	if err != nil {
		return
	}
	wg := sync.WaitGroup{}
	wg.Add(2)

	// Delete the temporary log group
	go func() {
		defer wg.Done()
		lib.DeleteLogGroup(sess, logGroup, conf.IsDebugMode)
	}()
	// Delete the temporary ECS cluster
	go func() {
		defer wg.Done()
		lib.DeleteECSCluster(sess, requestID, conf.IsDebugMode)
	}()
	wg.Wait()
}

var regTaskID = regexp.MustCompile("task/(.*)")

func outputRunResults(ctx context.Context, conf *RunConfig, startedAt, runTaskAt time.Time, logsAt *time.Time, logs map[string][]*cw.OutputLogEvent, taskdef *ecs.RegisterTaskDefinitionInput, runconfig *ecs.RunTaskInput, tasks []*ecs.Task) {
	result := map[string]interface{}{}

	if aws.BoolValue(conf.Asynchronous) { // Async mode
		result["RequestID"] = requestID
		if len(tasks) > 0 {
			resources := []map[string]string{}
			for _, task := range tasks {
				resource := map[string]string{}
				resource["TaskARN"] = aws.StringValue(task.TaskArn)
				resource["PublicIP"] = lib.RetrievePublicIP(
					ctx,
					conf.Aws.AccessKey,
					conf.Aws.SecretKey,
					conf.Aws.Region,
					task,
					conf.Common.IsDebugMode,
				)
				resources = append(resources, resource)
			}
			result["Tasks"] = resources
		}
	} else { // Sync mode
		seq := 1
		for _, value := range logs {
			messages := []string{}
			for _, event := range value {
				messages = append(messages, fmt.Sprintf(
					"%s: %s",
					time.Unix(aws.Int64Value(event.Timestamp)/1000, 0).Format(time.RFC3339),
					aws.StringValue(event.Message),
				))
			}
			result[fmt.Sprintf("container-%d", seq)] = messages
			seq++
		}
	}
	if aws.BoolValue(conf.Common.ExtendedOutput) {
		timelines := []map[string]string{}
		resources := []map[string]interface{}{}
		exitcodes := []map[string]interface{}{}
		if len(tasks) > 0 {
			for _, task := range tasks {
				resource := map[string]interface{}{}
				container := taskdef.ContainerDefinitions[0]
				resource["ClusterArn"] = aws.StringValue(task.ClusterArn)
				resource["TaskDefinitionArn"] = aws.StringValue(task.TaskDefinitionArn)
				resource["TaskArn"] = aws.StringValue(task.TaskArn)
				resource["TaskRoleArn"] = aws.StringValue(taskdef.TaskRoleArn)
				resource["LogGroup"] = aws.StringValue(container.LogConfiguration.Options["awslogs-group"])
				resource["PublicIP"] = lib.RetrievePublicIP(
					ctx,
					conf.Aws.AccessKey,
					conf.Aws.SecretKey,
					conf.Aws.Region,
					task,
					conf.Common.IsDebugMode,
				)
				containers := []map[string]interface{}{}
				taskID := ""
				matched := regTaskID.FindAllStringSubmatch(aws.StringValue(task.TaskArn), -1)
				if len(matched) > 0 && len(matched[0]) > 1 {
					taskID = matched[0][1]
				}
				for _, container := range task.Containers {
					containers = append(containers, map[string]interface{}{
						"ContainerArn": aws.StringValue(container.ContainerArn),
						"LogStream":    fmt.Sprintf("fargate/%s/%s", aws.StringValue(container.Name), taskID),
					})
				}
				resource["Containers"] = containers
				resources = append(resources, resource)

				timeline := map[string]string{}
				timeline["0"] = fmt.Sprintf("AppStartedAt:              %s", rfc3339(startedAt))
				timeline["1"] = fmt.Sprintf("AppTriedToRunFargateAt:    %s", rfc3339(runTaskAt))
				timeline["2"] = fmt.Sprintf("FargateCreatedAt:          %s", toStr(task.CreatedAt))
				timeline["3"] = fmt.Sprintf("FargatePullStartedAt:      %s", toStr(task.PullStartedAt))
				timeline["4"] = fmt.Sprintf("FargatePullStoppedAt:      %s", toStr(task.PullStoppedAt))
				timeline["5"] = fmt.Sprintf("FargateStartedAt:          %s", toStr(task.StartedAt))
				timeline["6"] = fmt.Sprintf("FargateExecutionStoppedAt: %s", toStr(task.ExecutionStoppedAt))
				timeline["7"] = fmt.Sprintf("FargateStoppedAt:          %s", toStr(task.StoppedAt))
				timeline["8"] = fmt.Sprintf("AppRetrievedLogsAt:        %s", rfc3339(aws.TimeValue(logsAt)))
				timeline["9"] = fmt.Sprintf("AppFinishedAt:             %s", rfc3339(time.Now()))
				timelines = append(timelines, timeline)

				exitcode := map[string]interface{}{
					"TaskLastStatus":    aws.StringValue(task.LastStatus),
					"TaskHealthStatus":  aws.StringValue(task.HealthStatus),
					"TaskStopCode":      aws.StringValue(task.StopCode),
					"TaskStoppedReason": aws.StringValue(task.StoppedReason),
				}
				containers = []map[string]interface{}{}
				for _, container := range task.Containers {
					containers = append(containers, map[string]interface{}{
						"ExitCode":     aws.Int64Value(container.ExitCode),
						"Reason":       aws.StringValue(container.Reason),
						"LastStatus":   aws.StringValue(container.LastStatus),
						"HealthStatus": aws.StringValue(container.HealthStatus),
					})
				}
				exitcode["Containers"] = containers
				exitcodes = append(exitcodes, exitcode)
			}
		}
		result["meta"] = map[string]interface{}{
			"1.taskdef":   taskdef,
			"2.runconfig": runconfig,
			"3.resources": resources,
			"4.timeline":  timelines,
			"5.exitcodes": exitcodes,
		}
	}
	log.PrintJSON(result)
}

func outputStopResults(ctx context.Context, conf *StopConfig, logs map[string][]*cw.OutputLogEvent, tasks []*ecs.Task) {
	result := map[string]interface{}{}
	seq := 1
	for _, value := range logs {
		messages := []string{}
		for _, event := range value {
			messages = append(messages, fmt.Sprintf(
				"%s: %s",
				time.Unix(aws.Int64Value(event.Timestamp)/1000, 0).Format(time.RFC3339),
				aws.StringValue(event.Message),
			))
		}
		result[fmt.Sprintf("container-%d", seq)] = messages
		seq++
	}
	if aws.BoolValue(conf.Common.ExtendedOutput) {
		timelines := []map[string]string{}
		resources := []map[string]interface{}{}
		exitcodes := []map[string]interface{}{}
		if len(tasks) > 0 {
			for _, task := range tasks {
				resource := map[string]interface{}{}
				resource["ClusterArn"] = aws.StringValue(task.ClusterArn)
				resource["TaskDefinitionArn"] = aws.StringValue(task.TaskDefinitionArn)
				resource["TaskArn"] = aws.StringValue(task.TaskArn)

				containers := []map[string]interface{}{}
				taskID := ""
				matched := regTaskID.FindAllStringSubmatch(aws.StringValue(task.TaskArn), -1)
				if len(matched) > 0 && len(matched[0]) > 1 {
					taskID = matched[0][1]
				}
				for _, container := range task.Containers {
					containers = append(containers, map[string]interface{}{
						"ContainerArn": aws.StringValue(container.ContainerArn),
						"LogStream":    fmt.Sprintf("fargate/%s/%s", aws.StringValue(container.Name), taskID),
					})
				}
				resource["Containers"] = containers
				resources = append(resources, resource)

				timeline := map[string]string{}
				timeline["1"] = fmt.Sprintf("FargateCreatedAt:          %s", toStr(task.CreatedAt))
				timeline["2"] = fmt.Sprintf("FargatePullStartedAt:      %s", toStr(task.PullStartedAt))
				timeline["3"] = fmt.Sprintf("FargatePullStoppedAt:      %s", toStr(task.PullStoppedAt))
				timeline["4"] = fmt.Sprintf("FargateStartedAt:          %s", toStr(task.StartedAt))
				timeline["5"] = fmt.Sprintf("FargateExecutionStoppedAt: %s", toStr(task.ExecutionStoppedAt))
				timeline["6"] = fmt.Sprintf("FargateStoppedAt:          %s", toStr(task.StoppedAt))
				timeline["7"] = fmt.Sprintf("AppFinishedAt:             %s", rfc3339(time.Now()))
				timelines = append(timelines, timeline)

				exitcode := map[string]interface{}{
					"TaskLastStatus":    aws.StringValue(task.LastStatus),
					"TaskHealthStatus":  aws.StringValue(task.HealthStatus),
					"TaskStopCode":      aws.StringValue(task.StopCode),
					"TaskStoppedReason": aws.StringValue(task.StoppedReason),
				}
				containers = []map[string]interface{}{}
				for _, container := range task.Containers {
					containers = append(containers, map[string]interface{}{
						"ExitCode":     aws.Int64Value(container.ExitCode),
						"Reason":       aws.StringValue(container.Reason),
						"LastStatus":   aws.StringValue(container.LastStatus),
						"HealthStatus": aws.StringValue(container.HealthStatus),
					})
				}
				exitcode["Containers"] = containers
				exitcodes = append(exitcodes, exitcode)
			}
		}
		result["meta"] = map[string]interface{}{
			"1.resources": resources,
			"2.timeline":  timelines,
			"3.exitcodes": exitcodes,
		}
	}
	log.PrintJSON(result)
}

func toStr(t *time.Time) string {
	if t == nil {
		return "---"
	}
	return rfc3339(aws.TimeValue(t))
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
