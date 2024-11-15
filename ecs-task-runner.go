package etr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/docker/distribution/reference"
	"github.com/google/uuid"
	config "github.com/pottava/ecs-task-runner/conf"
	lib "github.com/pottava/ecs-task-runner/internal/aws"
	"github.com/pottava/ecs-task-runner/internal/log"
	"github.com/pottava/ecs-task-runner/internal/util"
	"golang.org/x/sync/errgroup"
)

var (
	requestID  string
	logGroup   string
	taskDefARN *string
)

func init() {
	requestID = fmt.Sprintf("ecs-task-runner-%s", uuid.New().String())
	logGroup = fmt.Sprintf("/ecs/%s", requestID)
}

// Run runs the docker image on Amazon ECS
func Run(ctx context.Context, conf *config.RunConfig) (output *Output, err error) {
	startedAt := time.Now()

	if conf.Common.IsDebugMode {
		log.PrintJSON(conf)
	}
	// Check AWS credentials
	sess, err := lib.Session(conf.Aws, conf.Common.IsDebugMode)
	if err != nil {
		return &Output{ExitCode: exitWithError}, err
	}
	conf.Aws.AccountID, err = getAccountID(ctx, sess, conf.Common, conf.Aws)
	if err != nil {
		return &Output{ExitCode: exitWithError}, err
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
		return &Output{ExitCode: exitWithError}, err
	}
	// Check if the environment variables contain sensitive data
	conf.WithParamStore = containsSensitiveData(ctx, sess, conf)

	// Create AWS resources
	var taskDefInput *ecs.RegisterTaskDefinitionInput
	taskDefInput, err = createResouces(ctx, sess, conf, image, startedAt)
	if err != nil {
		DeleteResouces(conf.Aws, conf.Common, sess)
		return &Output{ExitCode: exitWithError}, err
	}
	// Run the ECS task
	runTaskAt := time.Now()
	tasks, runconfig, err := run(ctx, sess, conf)
	if err != nil {
		DeleteResouces(conf.Aws, conf.Common, sess)
		return &Output{ExitCode: exitWithError}, err
	}
	// Asynchronous job
	if aws.BoolValue(conf.Asynchronous) {
		// Wait for its start
		tasks, err = waitForTask(ctx, sess, conf.Common, tasks, func(task *ecs.Task) bool {
			return !strings.EqualFold(aws.StringValue(task.LastStatus), "PROVISIONING") &&
				!strings.EqualFold(aws.StringValue(task.LastStatus), "PENDING")
		})
		if err != nil {
			DeleteResouces(conf.Aws, conf.Common, sess)
			return &Output{ExitCode: exitWithError}, err
		}
		output = runResults(ctx, conf, startedAt, runTaskAt, nil, nil, taskDefInput, runconfig, tasks)
		if len(tasks) == 0 || len(tasks[0].Containers) == 0 {
			output.ExitCode = exitWithError
		}
		deleteResoucesImmediately(conf.Aws, conf.Common, sess)
		return output, nil
	}
	// Wait for its done
	tasks, err = waitForTask(ctx, sess, conf.Common, tasks, func(task *ecs.Task) bool {
		return strings.EqualFold(aws.StringValue(task.LastStatus), "STOPPED") && task.StoppedAt != nil
	})
	if err != nil {
		DeleteResouces(conf.Aws, conf.Common, sess)
		return &Output{ExitCode: exitWithError}, err
	}
	// Retrieve app log
	logs := lib.RetrieveLogs(ctx, sess, tasks, aws.StringValue(conf.Common.EcsCluster), logGroup, logPrefix)
	retrieveLogsAt := time.Now()

	// Delete AWS resources
	DeleteResouces(conf.Aws, conf.Common, sess)

	// Format the result
	output = runResults(ctx, conf, startedAt, runTaskAt, &retrieveLogsAt, logs, taskDefInput, runconfig, tasks)

	if len(tasks) == 0 || len(tasks[0].Containers) == 0 {
		return &Output{ExitCode: exitWithError}, nil
	}
	for _, task := range tasks {
		for _, container := range task.Containers {
			if aws.Int64Value(output.ExitCode) != 0 {
				break
			}
			output.ExitCode = container.ExitCode
		}
	}
	for _, status := range output.Meta.ExitCodes {
		if strings.Contains(status.StopCode, "Failed") {
			output.ExitCode = exitWithError
		}
	}
	return output, nil
}

// Stop stops the Fargate container on Amazon ECS
func Stop(ctx context.Context, conf *config.StopConfig) (output *Output, err error) {

	// Check AWS credentials
	sess, err := lib.Session(conf.Aws, conf.Common.IsDebugMode)
	if err != nil {
		return &Output{ExitCode: exitWithError}, err
	}
	conf.Aws.AccountID, err = getAccountID(ctx, sess, conf.Common, conf.Aws)
	if err != nil {
		return &Output{ExitCode: exitWithError}, err
	}
	// Ensure parameters
	requestID = aws.StringValue(conf.RequestID)
	logGroup = fmt.Sprintf("/ecs/%s", requestID)
	conf.Common.ClusterExisted = !util.IsEmpty(conf.Common.EcsCluster)
	if !conf.Common.ClusterExisted {
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
		return &Output{ExitCode: exitWithError}, err
	}
	// Stop the task
	tasks := []*ecs.Task{}
	if len(conf.TaskARNs) == 0 {
		conf.TaskARNs = all.TaskArns
	}
	for _, taskARN := range conf.TaskARNs {
		task, err := lib.StopTask(ctx, sess, conf.Common.EcsCluster, taskARN)
		if err != nil {
			return &Output{ExitCode: exitWithError}, err
		}
		tasks = append(tasks, task)
	}
	tasks, _ = waitForTask(ctx, sess, conf.Common, tasks, func(task *ecs.Task) bool { // nolint
		return strings.EqualFold(aws.StringValue(task.LastStatus), "STOPPED") && task.StoppedAt != nil
	})
	logs := lib.RetrieveLogs(ctx, sess, tasks, aws.StringValue(conf.Common.EcsCluster), logGroup, logPrefix)
	output = stopResults(ctx, conf, logs, tasks)

	// Delete AWS resources
	if len(all.TaskArns) == len(tasks) {
		deleteResoucesInTheEnd(conf.Aws, conf.Common, sess)
	}
	if len(tasks) == 0 || len(tasks[0].Containers) == 0 {
		return &Output{ExitCode: exitNormally}, nil
	}
	for _, task := range tasks {
		for _, container := range task.Containers {
			if aws.Int64Value(output.ExitCode) != 0 {
				break
			}
			output.ExitCode = container.ExitCode
		}
	}
	for _, status := range output.Meta.ExitCodes {
		if strings.Contains(status.StopCode, "Failed") {
			output.ExitCode = exitWithError
		}
	}
	return output, nil
}

func validateImageName(ctx context.Context, conf *config.RunConfig, sess *session.Session) (*string, error) {
	imageHost, imageName, imageTag, err := parseImageName(conf.Image)
	if err != nil {
		log.Errors.Println("Provided image name is invalid.")
		return nil, err
	}
	// Try to make up ECR image name
	if aws.BoolValue(conf.ForceECR) {
		if !strings.Contains(aws.StringValue(imageHost), conf.Aws.AccountID) {
			imageName = aws.String(fmt.Sprintf(
				"%s/%s",
				aws.StringValue(imageHost),
				aws.StringValue(imageName),
			))
			imageHost = aws.String(fmt.Sprintf(
				"%s.dkr.ecr.%s.amazonaws.com",
				conf.Aws.AccountID,
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

func ensureAWSResources(ctx context.Context, sess *session.Session, conf *config.RunConfig) error {
	eg, _ := errgroup.WithContext(context.Background())
	vpc := lib.FindDefaultVPC(ctx, sess)

	// Ensure cluster existence
	eg.Go(func() error {
		if util.IsEmpty(conf.Common.EcsCluster) {
			conf.Common.EcsCluster = aws.String(requestID)
		}
		existed, e := lib.CreateClusterIfNotExist(ctx, sess, conf.Common.EcsCluster, conf.Spot)
		conf.Common.ClusterExisted = existed
		return e
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

func containsSensitiveData(ctx context.Context, sess *session.Session, conf *config.RunConfig) bool {
	ssmParameterKey := fmt.Sprintf(
		"arn:aws:ssm:%s:%s:parameter/",
		aws.StringValue(conf.Aws.Region),
		conf.Aws.AccountID,
	)
	secretsManagerKey := fmt.Sprintf(
		"arn:aws:secretsmanager:%s:%s:secret:",
		aws.StringValue(conf.Aws.Region),
		conf.Aws.AccountID,
	)
	for _, val := range conf.Environments {
		if strings.HasPrefix(aws.StringValue(val), ssmParameterKey) || strings.HasPrefix(aws.StringValue(val), secretsManagerKey) {
			return true
		}
	}
	return false
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
	kmsCustomKeyID             = "\"arn:aws:kms:%s:%s:%s\","
	ecsGetParamsPolicyDocument = `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "kms:Decrypt",
      "ssm:GetParameters",
      "secretsmanager:GetSecretValue"
    ],
    "Resource": [
      %s
      "arn:aws:ssm:%s:%s:parameter/*",
	  "arn:aws:secretsmanager:%s:%s:secret:*"
    ]
  }]
}`
	fargate     = "FARGATE"
	fargateSpot = "FARGATE_SPOT"
	logPrefix   = "fargate"
	awsVPC      = "awsvpc"
	awsCWLogs   = "awslogs"
)

var (
	dockerCreds *string
	credsPolicy *string
)

func createResouces(ctx context.Context, sess *session.Session, conf *config.RunConfig, image *string, startedAt time.Time) (taskDefInput *ecs.RegisterTaskDefinitionInput, e error) {
	eg, _ := errgroup.WithContext(context.Background())

	eg.Go(func() error {
		// Make a temporary log group
		return lib.CreateLogGroup(ctx, sess, logGroup)
	})
	eg.Go(func() (err error) {
		if !util.IsEmpty(conf.DockerUser) {
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
		taskDefARN, taskDefInput, err = registerTaskDef(ctx, sess, conf, image, execRoleArn, startedAt)
		return
	})
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return taskDefInput, nil
}

func createIAMRole(ctx context.Context, sess *session.Session, conf *config.RunConfig) (*string, error) {
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
	// Or if you just want to specify sensitive data with AWS Systems Manager Parameter Store
	// https://docs.aws.amazon.com/AmazonECS/latest/developerguide/specifying-sensitive-data.html
	if (!util.IsEmpty(conf.DockerUser) && dockerCreds != nil) || conf.WithParamStore {
		policy, err := lib.CreatePolicy(
			ctx, sess,
			fmt.Sprintf("ecs-custom-%s", requestID),
			fmt.Sprintf(
				ecsGetParamsPolicyDocument,
				getKeyResourceName(ctx, sess, conf),
				aws.StringValue(conf.Aws.Region),
				conf.Aws.AccountID,
				aws.StringValue(conf.Aws.Region),
				conf.Aws.AccountID,
			))
		if err != nil {
			return nil, err
		}
		credsPolicy = policy.Arn
		if err = lib.AttachPolicy(ctx, sess, roleName, credsPolicy); err != nil {
			return nil, err
		}
		time.Sleep(5 * time.Second) // Lag in policy reflection is now observed, wait 5 seconds
	}
	return execRoleArn, nil
}

func getAccountID(ctx context.Context, sess *session.Session, conf *config.CommonConfig, awsdfg *config.AwsConfig) (string, error) {
	if awsdfg.AccountID != "" {
		return awsdfg.AccountID, nil
	}
	account, err := sts.New(sess).GetCallerIdentityWithContext(ctx, nil)
	if err != nil {
		return "", err
	}
	if conf.IsDebugMode {
		log.PrintJSON(account)
	}
	return aws.StringValue(account.Account), nil
}

func getKeyResourceName(ctx context.Context, sess *session.Session, conf *config.RunConfig) string {
	keyID := aws.StringValue(conf.KMSCustomKeyID)
	if keyID == "" {
		return ""
	}
	if strings.HasPrefix(keyID, "arn:aws:kms:") {
		return fmt.Sprintf("\"%s\",", keyID)
	}
	if _, check := uuid.Parse(keyID); check == nil {
		return fmt.Sprintf(
			kmsCustomKeyID,
			aws.StringValue(conf.Aws.Region),
			conf.Aws.AccountID,
			"key/"+keyID,
		)
	}
	// FIXME it doesn't work if you use alias
	// if strings.HasPrefix(keyID, "alias/") {
	// 	return fmt.Sprintf(
	// 		kmsCustomKeyID,
	// 		aws.StringValue(conf.Aws.Region),
	// 		conf.Aws.AccountID,
	// 		keyID,
	// 	)
	// }
	return ""
}

func registerTaskDef(ctx context.Context, sess *session.Session, conf *config.RunConfig, image, execRoleArn *string, startedAt time.Time) (*string, *ecs.RegisterTaskDefinitionInput, error) {
	ssmParameterKey := fmt.Sprintf(
		"arn:aws:ssm:%s:%s:parameter/",
		aws.StringValue(conf.Aws.Region),
		conf.Aws.AccountID,
	)
	secretsManagerKey := fmt.Sprintf(
		"arn:aws:secretsmanager:%s:%s:secret:",
		aws.StringValue(conf.Aws.Region),
		conf.Aws.AccountID,
	)
	environments := []*ecs.KeyValuePair{}
	secrets := []*ecs.Secret{}
	for key, val := range conf.Environments {
		if strings.HasPrefix(aws.StringValue(val), ssmParameterKey) || strings.HasPrefix(aws.StringValue(val), secretsManagerKey) {
			secrets = append(secrets, &ecs.Secret{
				Name:      aws.String(key),
				ValueFrom: val,
			})
		} else {
			environments = append(environments, &ecs.KeyValuePair{
				Name:  aws.String(key),
				Value: val,
			})
		}
	}
	ports := []*ecs.PortMapping{}
	for _, port := range conf.Ports {
		ports = append(ports, &ecs.PortMapping{
			ContainerPort: port,
		})
	}
	labels := map[string]*string{}
	labels["com.github.pottava.ecs-task-runner.version"] = aws.String(conf.Common.AppVersion)
	labels["com.github.pottava.ecs-task-runner.started"] = aws.String(rfc3339(startedAt))
	for key, value := range conf.Labels {
		labels[key] = value
	}
	containerDef := &ecs.ContainerDefinition{
		Name:         aws.String("app"),
		Image:        image,
		EntryPoint:   conf.Entrypoint,
		Command:      conf.Commands,
		Environment:  environments,
		Secrets:      secrets,
		PortMappings: ports,
		DockerLabels: labels,
		Essential:    aws.Bool(true),
		LogConfiguration: &ecs.LogConfiguration{
			LogDriver: aws.String(awsCWLogs),
			Options: map[string]*string{
				"awslogs-region":        conf.Aws.Region,
				"awslogs-group":         aws.String(logGroup),
				"awslogs-stream-prefix": aws.String(logPrefix),
			},
		},
		Privileged:             aws.Bool(false),
		ReadonlyRootFilesystem: conf.ReadOnlyRootFS,
	}
	if conf.User != nil && len(aws.StringValue(conf.User)) > 0 {
		containerDef.User = conf.User
	}
	if !util.IsEmpty(conf.DockerUser) && dockerCreds != nil {
		containerDef.RepositoryCredentials = &ecs.RepositoryCredentials{
			CredentialsParameter: dockerCreds,
		}
	}
	input := ecs.RegisterTaskDefinitionInput{
		Family:                  conf.TaskDefFamily,
		RequiresCompatibilities: []*string{aws.String(fargate)},
		ExecutionRoleArn:        execRoleArn,
		TaskRoleArn:             conf.TaskRoleArn,
		Cpu:                     conf.CPU,
		Memory:                  conf.Memory,
		NetworkMode:             aws.String(awsVPC),
		ContainerDefinitions:    []*ecs.ContainerDefinition{containerDef},
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

func run(ctx context.Context, sess *session.Session, conf *config.RunConfig) ([]*ecs.Task, *ecs.RunTaskInput, error) {
	assignPublicIP := "ENABLED"
	if !aws.BoolValue(conf.AssignPublicIP) {
		assignPublicIP = "DISABLED"
	}
	input := ecs.RunTaskInput{
		Cluster:        conf.Common.EcsCluster,
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
	if aws.BoolValue(conf.Spot) {
		input.CapacityProviderStrategy = []*ecs.CapacityProviderStrategyItem{
			&ecs.CapacityProviderStrategyItem{
				CapacityProvider: aws.String(fargateSpot),
				Base:             conf.NumberOfTasks,
				Weight:           aws.Int64(1),
			},
		}
	} else {
		input.LaunchType = aws.String(fargate)
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

func waitForTask(ctx context.Context, sess *session.Session, conf *config.CommonConfig, tasks []*ecs.Task, judge judgeFunc) ([]*ecs.Task, error) {
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
func DeleteResouces(aws *config.AwsConfig, conf *config.CommonConfig, sess *session.Session) {
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		deleteResoucesImmediately(aws, conf, sess)
	}()
	go func() {
		defer wg.Done()
		deleteResoucesInTheEnd(aws, conf, sess)
	}()
	wg.Wait()
}

func deleteResoucesImmediately(aws *config.AwsConfig, conf *config.CommonConfig, sess *session.Session) {
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
		if credsPolicy != nil {
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

func deleteResoucesInTheEnd(aws *config.AwsConfig, conf *config.CommonConfig, sess *session.Session) {
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
		if !conf.ClusterExisted {
			lib.DeleteECSCluster(sess, requestID, conf.IsDebugMode)
		}
	}()
	wg.Wait()
}
