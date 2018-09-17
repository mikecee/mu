package workflows

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/stelligent/mu/common"
)

var ecsImagePattern = "amzn-ami-*-amazon-ecs-optimized"
var ec2ImagePattern = "amzn-ami-hvm-*-x86_64-gp2"

// NewEnvironmentUpserter create a new workflow for upserting an environment
func NewEnvironmentUpserter(ctx *common.Context, environmentName string) Executor {

	workflow := new(environmentWorkflow)
	ecsStackParams := make(map[string]string)
	elbStackParams := make(map[string]string)
	workflow.codeRevision = ctx.Config.Repo.Revision
	workflow.repoName = ctx.Config.Repo.Slug

	return newPipelineExecutor(
		workflow.environmentFinder(&ctx.Config, environmentName),
		workflow.environmentRolesetUpserter(ctx.RolesetManager, ctx.RolesetManager, ecsStackParams),
		workflow.environmentVpcUpserter(ctx.Config.Namespace, ecsStackParams, elbStackParams, ctx.StackManager, ctx.StackManager, ctx.StackManager, ctx.StackManager),
		workflow.environmentElbUpserter(ctx.Config.Namespace, ecsStackParams, elbStackParams, ctx.StackManager, ctx.StackManager, ctx.StackManager),
		workflow.environmentUpserter(ctx.Config.Namespace, ecsStackParams, ctx.StackManager, ctx.StackManager, ctx.StackManager),
	)
}

// Find an environment in config, by name and set the reference
func (workflow *environmentWorkflow) environmentFinder(config *common.Config, environmentName string) Executor {

	return func() error {
		for _, e := range config.Environments {
			if strings.EqualFold(e.Name, environmentName) {
				if e.Provider == "" {
					e.Provider = common.EnvProviderEcs
				}
				workflow.environment = &e

				if e.Discovery.Provider == "consul" {
					return fmt.Errorf("Consul is no longer supported as a service discovery provider.  Check out the mu-consul extension for an alternative: https://github.com/stelligent/mu-consul")
				}

				return nil
			}
		}
		return common.Warningf("Unable to find environment named '%s' in configuration", environmentName)
	}
}

func (workflow *environmentWorkflow) environmentVpcUpserter(namespace string, ecsStackParams map[string]string, elbStackParams map[string]string, imageFinder common.ImageFinder, stackUpserter common.StackUpserter, stackWaiter common.StackWaiter, azCounter common.AZCounter) Executor {
	return func() error {
		environment := workflow.environment
		vpcStackParams := make(map[string]string)
		var err error

		var vpcStackName string
		var vpcTemplateName string
		if environment.VpcTarget.Environment != "" {
			targetNamespace := environment.VpcTarget.Namespace
			if targetNamespace == "" {
				targetNamespace = namespace
			}
			log.Debugf("VpcTarget exists for different environment; targeting that VPC")
			vpcStackName = common.CreateStackName(targetNamespace, common.StackTypeVpc, environment.VpcTarget.Environment)
		} else if environment.VpcTarget.VpcID != "" {
			log.Debugf("VpcTarget exists, so we will upsert the VPC stack that references the VPC attributes")
			vpcStackName = common.CreateStackName(namespace, common.StackTypeTarget, environment.Name)
			vpcTemplateName = "vpc-target.yml"

			// target VPC referenced from config
			vpcStackParams["VpcId"] = environment.VpcTarget.VpcID
			vpcStackParams["ElbSubnetIds"] = strings.Join(environment.VpcTarget.ElbSubnetIds, ",")
			vpcStackParams["InstanceSubnetIds"] = strings.Join(environment.VpcTarget.InstanceSubnetIds, ",")
		} else {
			log.Debugf("No VpcTarget, so we will upsert the VPC stack that manages the VPC")
			vpcStackName = common.CreateStackName(namespace, common.StackTypeVpc, environment.Name)
			vpcTemplateName = "vpc.yml"

			if environment.Cluster.InstanceTenancy != "" {
				vpcStackParams["InstanceTenancy"] = string(environment.Cluster.InstanceTenancy)
			}
			if environment.Cluster.SSHAllow != "" {
				vpcStackParams["SshAllow"] = environment.Cluster.SSHAllow
			} else {
				vpcStackParams["SshAllow"] = "0.0.0.0/0"
			}
			if environment.Cluster.KeyName != "" {
				vpcStackParams["BastionKeyName"] = environment.Cluster.KeyName
				vpcStackParams["BastionImageId"], err = imageFinder.FindLatestImageID(ec2ImagePattern)
				if err != nil {
					return err
				}
			}

			vpcStackParams["ElbInternal"] = strconv.FormatBool(environment.Loadbalancer.Internal)
		}

		azCount, err := azCounter.CountAZs()
		if err != nil {
			return err
		}
		if azCount < 2 {
			return fmt.Errorf("Only found %v availability zones...need at least 2", azCount)
		}
		vpcStackParams["AZCount"] = strconv.Itoa(azCount)

		if vpcTemplateName != "" {
			log.Noticef("Upserting VPC environment '%s' ...", environment.Name)

			tags := createTagMap(&EnvironmentTags{
				Environment: environment.Name,
				Type:        string(common.StackTypeVpc),
				Provider:    string(environment.Provider),
				Revision:    workflow.codeRevision,
				Repo:        workflow.repoName,
			})

			err = stackUpserter.UpsertStack(vpcStackName, vpcTemplateName, environment, vpcStackParams, tags, workflow.cloudFormationRoleArn)
			if err != nil {
				return err
			}

			log.Debugf("Waiting for stack '%s' to complete", vpcStackName)
			stack := stackWaiter.AwaitFinalStatus(vpcStackName)

			if stack == nil {
				return fmt.Errorf("Unable to create stack %s", vpcStackName)
			}
			if strings.HasSuffix(stack.Status, "ROLLBACK_COMPLETE") || !strings.HasSuffix(stack.Status, "_COMPLETE") {
				return fmt.Errorf("Ended in failed status %s %s", stack.Status, stack.StatusReason)
			}
		}

		ecsStackParams["VpcId"] = fmt.Sprintf("%s-VpcId", vpcStackName)
		ecsStackParams["InstanceSubnetIds"] = fmt.Sprintf("%s-InstanceSubnetIds", vpcStackName)

		elbStackParams["VpcId"] = fmt.Sprintf("%s-VpcId", vpcStackName)
		elbStackParams["ElbSubnetIds"] = fmt.Sprintf("%s-ElbSubnetIds", vpcStackName)

		return nil
	}
}

func (workflow *environmentWorkflow) environmentRolesetUpserter(rolesetUpserter common.RolesetUpserter, rolesetGetter common.RolesetGetter, ecsStackParams map[string]string) Executor {
	return func() error {
		err := rolesetUpserter.UpsertCommonRoleset()
		if err != nil {
			return err
		}

		commonRoleset, err := rolesetGetter.GetCommonRoleset()
		if err != nil {
			return err
		}

		workflow.cloudFormationRoleArn = commonRoleset["CloudFormationRoleArn"]

		err = rolesetUpserter.UpsertEnvironmentRoleset(workflow.environment.Name)
		if err != nil {
			return err
		}

		environmentRoleset, err := rolesetGetter.GetEnvironmentRoleset(workflow.environment.Name)
		if err != nil {
			return err
		}

		ecsStackParams["EC2InstanceProfileArn"] = environmentRoleset["EC2InstanceProfileArn"]

		return nil
	}
}

func (workflow *environmentWorkflow) environmentElbUpserter(namespace string, ecsStackParams map[string]string, elbStackParams map[string]string, imageFinder common.ImageFinder, stackUpserter common.StackUpserter, stackWaiter common.StackWaiter) Executor {
	return func() error {
		environment := workflow.environment
		envStackName := common.CreateStackName(namespace, common.StackTypeLoadBalancer, environment.Name)

		log.Noticef("Upserting ELB environment '%s' ...", environment.Name)

		stackParams := elbStackParams

		if environment.Loadbalancer.Certificate != "" {
			stackParams["ElbCert"] = environment.Loadbalancer.Certificate
		}

		if environment.Loadbalancer.HostedZone != "" {
			stackParams["ElbDomainName"] = environment.Loadbalancer.HostedZone

			if environment.Loadbalancer.Name == "" {
				// default to env name
				stackParams["ElbHostName"] = environment.Name
			} else {
				stackParams["ElbHostName"] = environment.Loadbalancer.Name
			}
		}

		if environment.Discovery.Name == "" {
			stackParams["ServiceDiscoveryName"] = fmt.Sprintf("%s.%s.local", environment.Name, namespace)
		} else {
			stackParams["ServiceDiscoveryName"] = environment.Discovery.Name
		}

		stackParams["ElbInternal"] = strconv.FormatBool(environment.Loadbalancer.Internal)

		tags := createTagMap(&EnvironmentTags{
			Environment: environment.Name,
			Type:        string(common.StackTypeLoadBalancer),
			Provider:    string(environment.Provider),
			Revision:    workflow.codeRevision,
			Repo:        workflow.repoName,
		})

		err := stackUpserter.UpsertStack(envStackName, "elb.yml", environment, stackParams, tags, workflow.cloudFormationRoleArn)
		if err != nil {
			return err
		}
		log.Debugf("Waiting for stack '%s' to complete", envStackName)
		stack := stackWaiter.AwaitFinalStatus(envStackName)

		if stack == nil {
			return fmt.Errorf("Unable to create stack %s", envStackName)
		}
		if strings.HasSuffix(stack.Status, "ROLLBACK_COMPLETE") || !strings.HasSuffix(stack.Status, "_COMPLETE") {
			return fmt.Errorf("Ended in failed status %s %s", stack.Status, stack.StatusReason)
		}

		ecsStackParams["ElbSecurityGroup"] = stack.Outputs["ElbInstanceSecurityGroup"]

		return nil
	}
}

func (workflow *environmentWorkflow) environmentUpserter(namespace string, ecsStackParams map[string]string,
	imageFinder common.ImageFinder, stackUpserter common.StackUpserter,
	stackWaiter common.StackWaiter) Executor {
	return func() error {
		log.Debugf("Using provider '%s' for environment", workflow.environment.Provider)

		environment := workflow.environment
		envStackName := common.CreateStackName(namespace, common.StackTypeEnv, environment.Name)

		stackParams := ecsStackParams

		var templateName string
		var imagePattern string
		envMapping := map[common.EnvProvider]map[string]string{
			common.EnvProviderEcs: map[string]string{
				"templateName": "env-ecs.yml",
				"imagePattern": ecsImagePattern,
				"launchType":   "EC2"},
			common.EnvProviderEcsFargate: map[string]string{
				"templateName": "env-ecs.yml",
				"imagePattern": ecsImagePattern,
				"launchType":   "FARGATE"},
			common.EnvProviderEc2: map[string]string{
				"templateName": "env-ec2.yml",
				"imagePattern": ec2ImagePattern,
				"launchType":   ""}}
		templateName = envMapping[environment.Provider]["templateName"]
		imagePattern = envMapping[environment.Provider]["imagePattern"]
		stackParams["LaunchType"] = envMapping[environment.Provider]["launchType"]

		log.Noticef("Upserting environment '%s' ...", environment.Name)

		if environment.Cluster.SSHAllow != "" {
			stackParams["SshAllow"] = environment.Cluster.SSHAllow
		} else {
			stackParams["SshAllow"] = "0.0.0.0/0"
		}
		if environment.Cluster.InstanceType != "" {
			stackParams["InstanceType"] = environment.Cluster.InstanceType
		}
		if environment.Cluster.ExtraUserData != "" {
			stackParams["ExtraUserData"] = environment.Cluster.ExtraUserData
		}
		if environment.Cluster.ImageID != "" {
			stackParams["ImageId"] = environment.Cluster.ImageID
		} else {
			var err error
			stackParams["ImageId"], err = imageFinder.FindLatestImageID(imagePattern)
			if err != nil {
				return err
			}

		}
		if environment.Cluster.ImageOsType != "" {
			stackParams["ImageOsType"] = environment.Cluster.ImageOsType
		}
		if environment.Cluster.DesiredCapacity != 0 {
			stackParams["DesiredCapacity"] = strconv.Itoa(environment.Cluster.DesiredCapacity)
		}
		if environment.Cluster.MinSize != 0 {
			stackParams["MinSize"] = strconv.Itoa(environment.Cluster.MinSize)
		}
		if environment.Cluster.MaxSize != 0 {
			stackParams["MaxSize"] = strconv.Itoa(environment.Cluster.MaxSize)
		}
		if environment.Cluster.KeyName != "" {
			stackParams["KeyName"] = environment.Cluster.KeyName
		}
		if environment.Cluster.TargetCPUReservation != 0 {
			stackParams["TargetCPUReservation"] = strconv.Itoa(environment.Cluster.TargetCPUReservation)
		}
		if environment.Cluster.TargetMemoryReservation != 0 {
			stackParams["TargetMemoryReservation"] = strconv.Itoa(environment.Cluster.TargetMemoryReservation)
		}
		if environment.Cluster.HTTPProxy != "" {
			stackParams["HttpProxy"] = environment.Cluster.HTTPProxy
		}

		tags := createTagMap(&EnvironmentTags{
			Environment: environment.Name,
			Type:        string(common.StackTypeEnv),
			Provider:    string(environment.Provider),
			Revision:    workflow.codeRevision,
			Repo:        workflow.repoName,
		})

		err := stackUpserter.UpsertStack(envStackName, templateName, environment, stackParams, tags, workflow.cloudFormationRoleArn)
		if err != nil {
			return err
		}
		log.Debugf("Waiting for stack '%s' to complete", envStackName)
		stack := stackWaiter.AwaitFinalStatus(envStackName)

		if stack == nil {
			return fmt.Errorf("Unable to create stack %s", envStackName)
		}
		if strings.HasSuffix(stack.Status, "ROLLBACK_COMPLETE") || !strings.HasSuffix(stack.Status, "_COMPLETE") {
			return fmt.Errorf("Ended in failed status %s %s", stack.Status, stack.StatusReason)
		}

		return nil
	}
}
