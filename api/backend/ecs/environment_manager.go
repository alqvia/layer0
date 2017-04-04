package ecsbackend

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"text/template"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/quintilesims/layer0/api/backend"
	"github.com/quintilesims/layer0/api/backend/ecs/id"
	"github.com/quintilesims/layer0/common/aws/autoscaling"
	"github.com/quintilesims/layer0/common/aws/ec2"
	"github.com/quintilesims/layer0/common/aws/ecs"
	"github.com/quintilesims/layer0/common/config"
	"github.com/quintilesims/layer0/common/errors"
	"github.com/quintilesims/layer0/common/models"
	"github.com/quintilesims/layer0/common/waitutils"
)

type ECSEnvironmentManager struct {
	ECS         ecs.Provider
	EC2         ec2.Provider
	AutoScaling autoscaling.Provider
	Backend     backend.Backend
	Clock       waitutils.Clock
}

func NewECSEnvironmentManager(
	ecsprovider ecs.Provider,
	ec2 ec2.Provider,
	asg autoscaling.Provider,
	backend backend.Backend) *ECSEnvironmentManager {

	return &ECSEnvironmentManager{
		ECS:         ecsprovider,
		EC2:         ec2,
		AutoScaling: asg,
		Backend:     backend,
		Clock:       waitutils.RealClock{},
	}
}

func (this *ECSEnvironmentManager) ListEnvironments() ([]*models.Environment, error) {
	clusters, err := this.ECS.Helper_DescribeClusters()
	if err != nil {
		return nil, err
	}

	environments := []*models.Environment{}
	for _, cluster := range clusters {
		if strings.HasPrefix(*cluster.ClusterName, id.PREFIX) {
			ecsEnvironmentID := id.ECSEnvironmentID(*cluster.ClusterName)
			environment := &models.Environment{
				EnvironmentID: ecsEnvironmentID.L0EnvironmentID(),
			}

			environments = append(environments, environment)
		}
	}

	return environments, nil
}

func (this *ECSEnvironmentManager) GetEnvironment(environmentID string) (*models.Environment, error) {
	ecsEnvironmentID := id.L0EnvironmentID(environmentID).ECSEnvironmentID()
	cluster, err := this.ECS.DescribeCluster(ecsEnvironmentID.String())
	if err != nil {
		if ContainsErrCode(err, "ClusterNotFoundException") || ContainsErrMsg(err, "cluster not found") {
			return nil, errors.Newf(errors.InvalidEnvironmentID, "Environment with id '%s' was not found", environmentID)
		}

		return nil, err
	}

	return this.populateModel(cluster)
}

func (this *ECSEnvironmentManager) populateModel(cluster *ecs.Cluster) (*models.Environment, error) {
	// assuming id.ECSEnvironmentID == ECSEnvironmentID.ClusterName()
	ecsEnvironmentID := id.ECSEnvironmentID(*cluster.ClusterName)

	var clusterCount int
	var instanceSize string
	var amiID string

	asg, err := this.describeAutoscalingGroup(ecsEnvironmentID)
	if err != nil {
		if ContainsErrMsg(err, "not found") {
			log.Errorf("Autoscaling Group for environment '%s' not found", ecsEnvironmentID)
		} else {
			return nil, err
		}
	}

	if asg != nil {
		clusterCount = len(asg.Instances)

		if asg.LaunchConfigurationName != nil {
			launchConfig, err := this.AutoScaling.DescribeLaunchConfiguration(*asg.LaunchConfigurationName)
			if err != nil {
				if ContainsErrMsg(err, "not found") {
					log.Errorf("Launch Config for environment '%s' not found", ecsEnvironmentID)
				} else {
					return nil, err
				}
			}

			if launchConfig != nil {
				instanceSize = *launchConfig.InstanceType
				amiID = *launchConfig.ImageId
			}
		}
	}

	var securityGroupID string
	securityGroup, err := this.EC2.DescribeSecurityGroup(ecsEnvironmentID.SecurityGroupName())
	if err != nil {
		return nil, err
	}

	if securityGroup != nil {
		securityGroupID = pstring(securityGroup.GroupId)
	}

	model := &models.Environment{
		EnvironmentID:   ecsEnvironmentID.L0EnvironmentID(),
		ClusterCount:    clusterCount,
		InstanceSize:    instanceSize,
		SecurityGroupID: securityGroupID,
		AMIID:           amiID,
	}

	return model, nil
}

func (this *ECSEnvironmentManager) describeAutoscalingGroup(ecsEnvironmentID id.ECSEnvironmentID) (*autoscaling.Group, error) {
	autoScalingGroupName := ecsEnvironmentID.AutoScalingGroupName()
	asg, err := this.AutoScaling.DescribeAutoScalingGroup(autoScalingGroupName)
	if err != nil {
		return nil, err
	}

	return asg, nil
}

func (this *ECSEnvironmentManager) CreateEnvironment(
	environmentName string,
	instanceSize string,
	operatingSystem string,
	amiID string,
	minClusterCount int,
	userDataTemplate []byte,
) (*models.Environment, error) {

	var defaultUserDataTemplate []byte
	var serviceAMI string
	switch strings.ToLower(operatingSystem) {
	case "linux":
		defaultUserDataTemplate = defaultLinuxUserDataTemplate
		serviceAMI = config.AWSLinuxServiceAMI()
	case "windows":
		defaultUserDataTemplate = defaultWindowsUserDataTemplate
		serviceAMI = config.AWSWindowsServiceAMI()
	default:
		return nil, fmt.Errorf("Operating system '%s' is not recognized", operatingSystem)
	}

	environmentID := id.GenerateHashedEntityID(environmentName)
	ecsEnvironmentID := id.L0EnvironmentID(environmentID).ECSEnvironmentID()

	if len(userDataTemplate) == 0 {
		userDataTemplate = defaultUserDataTemplate
	}

	if amiID != "" {
		serviceAMI = amiID
	}

	userData, err := renderUserData(ecsEnvironmentID, userDataTemplate)
	if err != nil {
		return nil, err
	}

	cluster, err := this.ECS.CreateCluster(ecsEnvironmentID.String())
	if err != nil {
		return nil, err
	}

	description := "Auto-generated Layer0 Environment Security Group"
	vpcID := config.AWSVPCID()

	groupID, err := this.EC2.CreateSecurityGroup(ecsEnvironmentID.SecurityGroupName(), description, vpcID)
	if err != nil {
		return nil, err
	}

	// wait for security group to propagate
	this.Clock.Sleep(time.Second * 2)
	if err := this.EC2.AuthorizeSecurityGroupIngressFromGroup(groupID, groupID); err != nil {
		return nil, err
	}

	agentGroupID := config.AWSAgentGroupID()
	securityGroups := []*string{groupID, &agentGroupID}
	ecsRole := config.AWSECSInstanceProfile()
	keyPair := config.AWSKeyPair()
	launchConfigurationName := ecsEnvironmentID.LaunchConfigurationName()

	if err := this.AutoScaling.CreateLaunchConfiguration(
		&launchConfigurationName,
		&serviceAMI,
		&ecsRole,
		&instanceSize,
		&keyPair,
		&userData,
		securityGroups,
	); err != nil {
		return nil, err
	}

	maxClusterCount := 0
	if minClusterCount > 0 {
		maxClusterCount = minClusterCount
	}

	if err := this.AutoScaling.CreateAutoScalingGroup(
		ecsEnvironmentID.AutoScalingGroupName(),
		launchConfigurationName,
		config.AWSPrivateSubnets(),
		minClusterCount,
		maxClusterCount,
	); err != nil {
		return nil, err
	}

	return this.populateModel(cluster)
}

func (this *ECSEnvironmentManager) UpdateEnvironment(environmentID string, minClusterCount int) (*models.Environment, error) {
	model, err := this.GetEnvironment(environmentID)
	if err != nil {
		return nil, err
	}

	if err := this.updateEnvironmentMinCount(model, minClusterCount); err != nil {
		return nil, err
	}

	return model, nil
}

func (this *ECSEnvironmentManager) updateEnvironmentMinCount(model *models.Environment, minClusterCount int) error {
	ecsEnvironmentID := id.L0EnvironmentID(model.EnvironmentID).ECSEnvironmentID()
	autoScalingGroupName := ecsEnvironmentID.AutoScalingGroupName()

	asg, err := this.describeAutoscalingGroup(ecsEnvironmentID)
	if err != nil {
		return err
	}

	if int(*asg.MaxSize) < minClusterCount {
		if err := this.AutoScaling.UpdateAutoScalingGroupMaxSize(autoScalingGroupName, minClusterCount); err != nil {
			return err
		}
	}

	if err := this.AutoScaling.UpdateAutoScalingGroupMinSize(autoScalingGroupName, minClusterCount); err != nil {
		return err
	}

	return nil
}

func (this *ECSEnvironmentManager) DeleteEnvironment(environmentID string) error {
	ecsEnvironmentID := id.L0EnvironmentID(environmentID).ECSEnvironmentID()

	autoScalingGroupName := ecsEnvironmentID.AutoScalingGroupName()
	if err := this.AutoScaling.UpdateAutoScalingGroupMinSize(autoScalingGroupName, 0); err != nil {
		if !ContainsErrMsg(err, "name not found") && !ContainsErrMsg(err, "is pending delete") {
			return err
		}
	}

	if err := this.AutoScaling.UpdateAutoScalingGroupMaxSize(autoScalingGroupName, 0); err != nil {
		if !ContainsErrMsg(err, "name not found") && !ContainsErrMsg(err, "is pending delete") {
			return err
		}
	}

	if err := this.AutoScaling.DeleteAutoScalingGroup(&autoScalingGroupName); err != nil {
		if !ContainsErrMsg(err, "name not found") {
			return err
		}
	}

	launchConfigurationName := ecsEnvironmentID.LaunchConfigurationName()
	if err := this.AutoScaling.DeleteLaunchConfiguration(&launchConfigurationName); err != nil {
		if !ContainsErrMsg(err, "name not found") {
			return err
		}
	}

	if err := this.waitForAutoScalingGroupInactive(ecsEnvironmentID); err != nil {
		return err
	}

	securityGroup, err := this.EC2.DescribeSecurityGroup(ecsEnvironmentID.SecurityGroupName())
	if err != nil {
		return err
	}

	if securityGroup != nil {
		if err := this.waitForSecurityGroupDeleted(securityGroup); err != nil {
			return err
		}
	}

	if err := this.ECS.DeleteCluster(ecsEnvironmentID.String()); err != nil {
		if !ContainsErrCode(err, "ClusterNotFoundException") {
			return err
		}
	}

	return nil
}

func (this *ECSEnvironmentManager) CreateEnvironmentLink(sourceEnvironmentID, destEnvironmentID string) error {
	sourceECSID := id.L0EnvironmentID(sourceEnvironmentID).ECSEnvironmentID()
	destECSID := id.L0EnvironmentID(destEnvironmentID).ECSEnvironmentID()

	sourceGroup, err := this.getEnvironmentSecurityGroup(sourceECSID)
	if err != nil {
		return err
	}

	destGroup, err := this.getEnvironmentSecurityGroup(destECSID)
	if err != nil {
		return err
	}

	if err := this.EC2.AuthorizeSecurityGroupIngressFromGroup(sourceGroup.GroupId, destGroup.GroupId); err != nil {
		if !ContainsErrCode(err, "InvalidPermission.Duplicate") {
			return err
		}
	}

	if err := this.EC2.AuthorizeSecurityGroupIngressFromGroup(destGroup.GroupId, sourceGroup.GroupId); err != nil {
		if !ContainsErrCode(err, "InvalidPermission.Duplicate") {
			return err
		}
	}

	return nil
}

func (this *ECSEnvironmentManager) getEnvironmentSecurityGroup(environmentID id.ECSEnvironmentID) (*ec2.SecurityGroup, error) {
	group, err := this.EC2.DescribeSecurityGroup(environmentID.SecurityGroupName())
	if err != nil {
		return nil, err
	}

	if group == nil {
		return nil, fmt.Errorf("Security group for environment '%s' does not exist", environmentID.L0EnvironmentID())
	}

	return group, nil
}

func (this *ECSEnvironmentManager) waitForAutoScalingGroupInactive(ecsEnvironmentID id.ECSEnvironmentID) error {
	autoScalingGroupName := ecsEnvironmentID.AutoScalingGroupName()

	check := func() (bool, error) {
		group, err := this.AutoScaling.DescribeAutoScalingGroup(autoScalingGroupName)
		if err != nil {
			if ContainsErrMsg(err, "not found") {
				return true, nil
			}

			return false, err
		}

		log.Debugf("Waiting for ASG %s to delete (status: '%s')", autoScalingGroupName, pstring(group.Status))
		return false, nil
	}

	waiter := waitutils.Waiter{
		Name:    fmt.Sprintf("Stop Autoscaling %s", autoScalingGroupName),
		Retries: 50,
		Delay:   time.Second * 10,
		Clock:   this.Clock,
		Check:   check,
	}

	return waiter.Wait()
}

func (this *ECSEnvironmentManager) waitForSecurityGroupDeleted(securityGroup *ec2.SecurityGroup) error {
	check := func() (bool, error) {
		if err := this.EC2.DeleteSecurityGroup(securityGroup); err == nil {
			return true, nil
		}

		return false, nil
	}

	waiter := waitutils.Waiter{
		Name:    fmt.Sprintf("SecurityGroup delete for '%v'", securityGroup),
		Retries: 50,
		Delay:   time.Second * 10,
		Clock:   this.Clock,
		Check:   check,
	}

	return waiter.Wait()
}

func renderUserData(ecsEnvironmentID id.ECSEnvironmentID, userData []byte) (string, error) {
	tmpl, err := template.New("").Parse(string(userData))
	if err != nil {
		return "", fmt.Errorf("Failed to parse user data: %v", err)
	}

	context := struct {
		ECSEnvironmentID string
		S3Bucket         string
	}{
		ECSEnvironmentID: ecsEnvironmentID.String(),
		S3Bucket:         config.AWSS3Bucket(),
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, context); err != nil {
		return "", fmt.Errorf("Failed to render user data: %v", err)
	}

	return base64.StdEncoding.EncodeToString(rendered.Bytes()), nil
}

var defaultWindowsUserDataTemplate = []byte(
	`<powershell>
# Set agent env variables for the Machine context (durable)
$clusterName = "{{ .ECSEnvironmentID }}"
Write-Host Cluster name set as: $clusterName -foreground green

[Environment]::SetEnvironmentVariable("ECS_CLUSTER", $clusterName, "Machine")
[Environment]::SetEnvironmentVariable("ECS_ENABLE_TASK_IAM_ROLE", "false", "Machine")
$agentVersion = 'v1.14.0-1.windows.1'
$agentZipUri = "https://s3.amazonaws.com/amazon-ecs-agent/ecs-agent-windows-$agentVersion.zip"
$agentZipMD5Uri = "$agentZipUri.md5"

# Configure docker auth
Read-S3Object -BucketName {{ .S3Bucket }} -Key bootstrap/dockercfg -File dockercfg.json
$dockercfgContent = [IO.File]::ReadAllText("dockercfg.json")
[Environment]::SetEnvironmentVariable("ECS_ENGINE_AUTH_DATA", $dockercfgContent, "Machine")
[Environment]::SetEnvironmentVariable("ECS_ENGINE_AUTH_TYPE", "dockercfg", "Machine")

### --- Nothing user configurable after this point ---
$ecsExeDir = "$env:ProgramFiles\Amazon\ECS"
$zipFile = "$env:TEMP\ecs-agent.zip"
$md5File = "$env:TEMP\ecs-agent.zip.md5"

### Get the files from S3
Invoke-RestMethod -OutFile $zipFile -Uri $agentZipUri
Invoke-RestMethod -OutFile $md5File -Uri $agentZipMD5Uri

## MD5 Checksum
$expectedMD5 = (Get-Content $md5File)
$md5 = New-Object -TypeName System.Security.Cryptography.MD5CryptoServiceProvider
$actualMD5 = [System.BitConverter]::ToString($md5.ComputeHash([System.IO.File]::ReadAllBytes($zipFile))).replace('-', '')

if($expectedMD5 -ne $actualMD5) {
    echo "Download doesn't match hash."
    echo "Expected: $expectedMD5 - Got: $actualMD5"
    exit 1
}

## Put the executables in the executable directory.
Expand-Archive -Path $zipFile -DestinationPath $ecsExeDir -Force

## Start the agent script in the background.
$jobname = "ECS-Agent-Init"
$script =  "cd '$ecsExeDir'; .\amazon-ecs-agent.ps1"
$repeat = (New-TimeSpan -Minutes 1)

$jobpath = $env:LOCALAPPDATA + "\Microsoft\Windows\PowerShell\ScheduledJobs\$jobname\ScheduledJobDefinition.xml"
if($(Test-Path -Path $jobpath)) {
  echo "Job definition already present"
  exit 0

}

$scriptblock = [scriptblock]::Create("$script")
$trigger = New-JobTrigger -At (Get-Date).Date -RepeatIndefinitely -RepetitionInterval $repeat -Once
$options = New-ScheduledJobOption -RunElevated -ContinueIfGoingOnBattery -StartIfOnBattery
Register-ScheduledJob -Name $jobname -ScriptBlock $scriptblock -Trigger $trigger -ScheduledJobOption $options -RunNow
Add-JobTrigger -Name $jobname -Trigger (New-JobTrigger -AtStartup -RandomDelay 00:1:00)
</powershell>
<persist>true</persist>
`)

var defaultLinuxUserDataTemplate = []byte(
	`#!/bin/bash
    echo ECS_CLUSTER={{ .ECSEnvironmentID }} >> /etc/ecs/ecs.config
    echo ECS_ENGINE_AUTH_TYPE=dockercfg >> /etc/ecs/ecs.config
    yum install -y aws-cli awslogs jq
    aws s3 cp s3://{{ .S3Bucket }}/bootstrap/dockercfg dockercfg
    cfg=$(cat dockercfg)
    echo ECS_ENGINE_AUTH_DATA=$cfg >> /etc/ecs/ecs.config
    docker pull amazon/amazon-ecs-agent:latest
    start ecs
`)
