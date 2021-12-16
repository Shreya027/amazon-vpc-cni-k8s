package upgrade

import (
	"context"
	"fmt"
	k8sUtils "github.com/aws/amazon-vpc-cni-k8s/test/framework/resources/k8s/utils"
	"github.com/aws/aws-sdk-go/service/eks"
	"time"

	"github.com/aws/amazon-vpc-cni-k8s/test/framework/resources/k8s/manifest"
	"github.com/aws/amazon-vpc-cni-k8s/test/framework/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	appsV1 "k8s.io/api/apps/v1"
	batchV1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
)

const (
	serviceLabelSelectorKey = "role"
	serviceLabelSelectorVal = "service-test"
)

var _ = Describe("test  CNI upgrade", func() {

	var (
		describeAddonOutput *eks.DescribeAddonOutput
		err                 error
	)

	It("should successfully run on initial addon version", func() {
		By("getting the current addon")
		describeAddonOutput, err = f.CloudServices.EKS().DescribeAddon("vpc-cni", f.Options.ClusterName)
		if err == nil {

			if *describeAddonOutput.Addon.AddonVersion != initialCNIVersion {
				By("apply initial addon version")
				_, err = f.CloudServices.EKS().CreateAddonWithVersion("vpc-cni", f.Options.ClusterName, initialCNIVersion)
				Expect(err).ToNot(HaveOccurred())

			}
		} else {
			By("apply initial addon version")
			_, err = f.CloudServices.EKS().CreateAddonWithVersion("vpc-cni", f.Options.ClusterName, initialCNIVersion)
			Expect(err).ToNot(HaveOccurred())
		}

		var status string = ""

		By("getting the initial addon...")
		for status != "ACTIVE" {
			describeAddonOutput, err = f.CloudServices.EKS().DescribeAddon("vpc-cni", f.Options.ClusterName)
			Expect(err).ToNot(HaveOccurred())
			status = *describeAddonOutput.Addon.Status
		}
		//Set the WARM_ENI_TARGET to 0 to prevent all pods being scheduled on secondary ENI
		k8sUtils.AddEnvVarToDaemonSetAndWaitTillUpdated(f, "aws-node", "kube-system",
			"aws-node", map[string]string{"WARM_IP_TARGET": "3", "WARM_ENI_TARGET": "0"})

	})

	Context("when testing pod traffic on initial version", func() {
		testServiceConnectivity()
	})

	It("should successfully run on final addon version", func() {

		By("getting the current addon")
		describeAddonOutput, err = f.CloudServices.EKS().DescribeAddon("vpc-cni", f.Options.ClusterName)

		if err == nil {

			if *describeAddonOutput.Addon.AddonVersion != finalCNIVersion {
				By("apply final addon version")
				_, err = f.CloudServices.EKS().CreateAddonWithVersion("vpc-cni", f.Options.ClusterName, finalCNIVersion)
				Expect(err).ToNot(HaveOccurred())
			}
		} else {
			By("apply final addon version")
			_, err = f.CloudServices.EKS().CreateAddonWithVersion("vpc-cni", f.Options.ClusterName, finalCNIVersion)
			Expect(err).ToNot(HaveOccurred())
		}

		var status string = ""

		By("getting the final addon...")
		for status != "ACTIVE" {
			describeAddonOutput, err = f.CloudServices.EKS().DescribeAddon("vpc-cni", f.Options.ClusterName)
			Expect(err).ToNot(HaveOccurred())
			status = *describeAddonOutput.Addon.Status
		}
		//Set the WARM_ENI_TARGET to 0 to prevent all pods being scheduled on secondary ENI
		k8sUtils.AddEnvVarToDaemonSetAndWaitTillUpdated(f, "aws-node", "kube-system",
			"aws-node", map[string]string{"WARM_IP_TARGET": "3", "WARM_ENI_TARGET": "0"})

	})

	Context("when testing pod traffic on final version", func() {
		testServiceConnectivity()
	})

})

func testServiceConnectivity() {
	var err error

	// Deployment running the http server
	var deployment *appsV1.Deployment
	var deploymentContainer v1.Container

	// Service front ending the http server deployment
	var service *v1.Service
	var serviceType v1.ServiceType
	var serviceAnnotation map[string]string

	// Test job that verifies connectivity to the http server
	// by querying the service URL
	var testerJob *batchV1.Job
	var testerContainer v1.Container

	// Test job that verifies test fails when connecting to
	// a non reachable port/address
	var negativeTesterJob *batchV1.Job
	var negativeTesterContainer v1.Container

	JustBeforeEach(func() {
		deploymentContainer = manifest.NewBusyBoxContainerBuilder().
			Image("nginx:1.21.4").
			Command(nil).
			Port(v1.ContainerPort{
				ContainerPort: 80,
				Protocol:      "TCP",
			}).Build()

		deployment = manifest.NewDefaultDeploymentBuilder().
			Name("http-server").
			Container(deploymentContainer).
			Replicas(20).
			PodLabel(serviceLabelSelectorKey, serviceLabelSelectorVal).
			Build()

		By("creating and waiting for deployment to be ready")
		deployment, err = f.K8sResourceManagers.DeploymentManager().
			CreateAndWaitTillDeploymentIsReady(deployment, utils.DefaultDeploymentReadyTimeout)
		Expect(err).ToNot(HaveOccurred())

		service = manifest.NewHTTPService().
			ServiceType(serviceType).
			Name("test-service").
			Selector(serviceLabelSelectorKey, serviceLabelSelectorVal).
			Annotations(serviceAnnotation).
			Build()

		By(fmt.Sprintf("creating the service of type %s", serviceType))
		service, err = f.K8sResourceManagers.ServiceManager().
			CreateService(context.Background(), service)
		Expect(err).ToNot(HaveOccurred())

		fmt.Fprintf(GinkgoWriter, "created service\n: %+v\n", service.Status)

		By("sleeping for some time to allow service to become ready")
		time.Sleep(utils.PollIntervalLong)

		testerContainer = manifest.NewBusyBoxContainerBuilder().
			Command([]string{"wget"}).
			Args([]string{"--spider", "-T", "5", fmt.Sprintf("%s:%d", service.Spec.ClusterIP,
				service.Spec.Ports[0].Port)}).
			Build()

		testerJob = manifest.NewDefaultJobBuilder().
			Parallelism(20).
			Container(testerContainer).
			Build()

		By("creating jobs to verify service connectivity")
		_, err = f.K8sResourceManagers.JobManager().
			CreateAndWaitTillJobCompleted(testerJob)
		Expect(err).ToNot(HaveOccurred())

		// Test connection to an unreachable port should fail
		negativeTesterContainer = manifest.NewBusyBoxContainerBuilder().
			Command([]string{"wget"}).
			Args([]string{"--spider", "-T", "1", fmt.Sprintf("%s:%d", service.Spec.ClusterIP, 2273)}).
			Build()

		negativeTesterJob = manifest.NewDefaultJobBuilder().
			Name("negative-test-job").
			Parallelism(2).
			Container(negativeTesterContainer).
			Build()

		By("creating negative jobs to verify service connectivity fails for unreachable port")
		_, err = f.K8sResourceManagers.JobManager().
			CreateAndWaitTillJobCompleted(negativeTesterJob)
		Expect(err).To(HaveOccurred())
	})

	JustAfterEach(func() {
		err := f.K8sResourceManagers.JobManager().DeleteAndWaitTillJobIsDeleted(testerJob)
		Expect(err).ToNot(HaveOccurred())

		err = f.K8sResourceManagers.JobManager().DeleteAndWaitTillJobIsDeleted(negativeTesterJob)
		Expect(err).ToNot(HaveOccurred())

		err = f.K8sResourceManagers.ServiceManager().DeleteAndWaitTillServiceDeleted(context.Background(), service)
		Expect(err).ToNot(HaveOccurred())

		err = f.K8sResourceManagers.DeploymentManager().DeleteAndWaitTillDeploymentIsDeleted(deployment)
		Expect(err).ToNot(HaveOccurred())
	})

	Context("when a deployment behind clb service is created", func() {
		BeforeEach(func() {
			serviceType = v1.ServiceTypeLoadBalancer
		})

		It("clb service pod should be reachable", func() {})
	})

	Context("when a deployment behind nlb service is created", func() {
		BeforeEach(func() {
			serviceType = v1.ServiceTypeLoadBalancer
			serviceAnnotation = map[string]string{"service.beta.kubernetes.io/" +
				"aws-load-balancer-type": "nlb"}
		})

		It("nlb service pod should be reachable", func() {})
	})

	Context("when a deployment behind cluster IP is created", func() {
		BeforeEach(func() {
			serviceType = v1.ServiceTypeClusterIP
		})

		It("clusterIP service pod should be reachable", func() {})
	})

	Context("when a deployment behind node port is created", func() {
		BeforeEach(func() {
			serviceType = v1.ServiceTypeNodePort
		})

		It("node port service pod should be reachable", func() {})
	})

}
