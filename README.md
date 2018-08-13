# A synchronous task runner for AWS Fargate on Amazon ECS

[![pottava/ecs-task-runner](http://dockeri.co/image/pottava/ecs-task-runner)](https://hub.docker.com/r/pottava/ecs-task-runner/)

Supported tags and respective `Dockerfile` links:  
・latest ([Dockerfile](https://github.com/pottava/ecs-task-runner/blob/master/Dockerfile))  
・1 ([Dockerfile](https://github.com/pottava/ecs-task-runner/blob/master/Dockerfile))  


## Description

This is a synchronous task runner for AWS Fargate. It runs a docker container on Fargate and waits for its done. Then it returns its standard output logs from CloudWatch Logs. All resources we need are created temporarily and remove them after the task finished.


## Installation

go:

```
$ go get github.com/pottava/ecs-task-runner/...
```

docker:

```
$ docker pull pottava/ecs-task-runner:1
```


## Parameters

Environment Variables     | Argument        | Description                     | Required | Default 
------------------------- | --------------- | ------------------------------- | -------- | ---------
DOCKER_IMAGE              | image           | Docker image to be run on ECS   | *        |
AWS_ACCESS_KEY_ID         | access_key      | AWS `access key` for API access | *        |
AWS_SECRET_ACCESS_KEY     | secret_key      | AWS `secret key` for API access | *        |
AWS_DEFAULT_REGION        | region          | AWS `region` for API access     |          | us-east-1
ECS_CLUSTER               | cluster         | Amazon ECS cluster name         |          | default
SUBNETS                   | subnets         | Fargate's Subnets               | *        |
SECURITY_GROUPS           | security_groups | Fargate's SecurityGroups        | *        |
CPU                       | cpu             | Requested vCPU to run Fargate   |          | 256
MEMORY                    | memory          | Requested memory to run Fargate |          | 512
NUMBER                    | number          | Number of tasks                 |          | 1 
TASK_TIMEOUT              | timeout         | Timeout minutes for the task    |          | 30


## Sample

With arguments:

```console
$ ecs-task-runner -a AKIAIOSFODNN7EXAMPLE -s wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY run -c sample-cluster -i sample/image:test --subnets subnet-xxx --security_groups sg-yyy --security_groups sg-zzz
{
  "container-1": [
    "2018-08-15T12:01:26+09:00: Hello world!",
    "2018-08-15T12:05:01+09:00: message 1",
    "2018-08-15T12:07:01+09:00: message 2"
  ]
}
```

With environment variables:

```console
$ export DOCKER_IMAGE
$ export AWS_ACCESS_KEY_ID
$ ..
$ ecs-task-runner run
```

With docker container:

```console
$ export DOCKER_IMAGE
$ export AWS_ACCESS_KEY_ID
$ ..
$ docker run --rm -e DOCKER_IMAGE -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_DEFAULT_REGION -e ECS_CLUSTER -e SUBNETS -e SECURITY_GROUPS pottava/ecs-task-runner:1 run
```
