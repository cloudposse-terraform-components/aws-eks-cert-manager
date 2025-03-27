package test

import (
	"context"
	"testing"
	"fmt"
	"time"
	"strings"
	helper "github.com/cloudposse/test-helpers/pkg/atmos/component-helper"
	awsHelper "github.com/cloudposse/test-helpers/pkg/aws"
	"github.com/cloudposse/test-helpers/pkg/atmos"
	"github.com/cloudposse/test-helpers/pkg/helm"
	// "github.com/gruntwork-io/terratest/modules/aws"
	"github.com/stretchr/testify/assert"
	"github.com/gruntwork-io/terratest/modules/random"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/client-go/dynamic"

)

type ComponentSuite struct {
	helper.TestSuite
}

func (s *ComponentSuite) TestBasic() {
	const component = "eks/cert-manager/basic"
	const stack = "default-test"
	const awsRegion = "us-east-2"

	clusterOptions := s.GetAtmosOptions("eks/cluster", stack, nil)
	clusrerId := atmos.Output(s.T(), clusterOptions, "eks_cluster_id")
	cluster := awsHelper.GetEksCluster(s.T(), context.Background(), awsRegion, clusrerId)

	dnsDelegatedOptions := s.GetAtmosOptions("dns-delegated", stack, nil)
	delegatedDomainName := atmos.Output(s.T(), dnsDelegatedOptions, "default_domain_name")

	randomID := strings.ToLower(random.UniqueId())
	namespace := fmt.Sprintf("cert-manager-%s", randomID)
	certName := fmt.Sprintf("cert-%s", randomID)
	domainName := fmt.Sprintf("%s.%s", randomID, delegatedDomainName)

	inputs := map[string]interface{}{
		"kubernetes_namespace": namespace,
		"cert_manager_issuer_support_email_template": fmt.Sprintf("aws-%s+%s@%s", randomID, "%s", delegatedDomainName),
	}

	defer s.DestroyAtmosComponent(s.T(), component, stack, &inputs)
	options, _ := s.DeployAtmosComponent(s.T(), component, stack, &inputs)
	assert.NotNil(s.T(), options)

	metadataCertManager := helm.Metadata{}

	atmos.OutputStruct(s.T(), options, "cert_manager_metadata", &metadataCertManager)

	assert.Equal(s.T(), metadataCertManager.AppVersion, "v1.5.4")
	assert.Equal(s.T(), metadataCertManager.Chart, "cert-manager")
	assert.NotNil(s.T(), metadataCertManager.FirstDeployed)
	assert.NotNil(s.T(), metadataCertManager.LastDeployed)
	assert.Equal(s.T(), metadataCertManager.Name, "cert-manager")
	assert.Equal(s.T(), metadataCertManager.Namespace, namespace)
	assert.NotEmpty(s.T(), metadataCertManager.Notes)
	assert.Equal(s.T(), metadataCertManager.Revision, 1)
	assert.NotNil(s.T(), metadataCertManager.Values)
	assert.Equal(s.T(), metadataCertManager.Version, "v1.5.4")


	metadataCertManagerIssuer := helm.Metadata{}

	atmos.OutputStruct(s.T(), options, "cert_manager_issuer_metadata", &metadataCertManagerIssuer)

	assert.Equal(s.T(), metadataCertManagerIssuer.AppVersion, "1.0.0")
	assert.Equal(s.T(), metadataCertManagerIssuer.Chart, "cert-manager-issuer")
	assert.NotNil(s.T(), metadataCertManagerIssuer.FirstDeployed)
	assert.NotNil(s.T(), metadataCertManagerIssuer.LastDeployed)
	assert.Equal(s.T(), metadataCertManagerIssuer.Name, "cert-manager-issuer")
	assert.Equal(s.T(), metadataCertManagerIssuer.Namespace, namespace)
	assert.Empty(s.T(), metadataCertManagerIssuer.Notes)
	assert.Equal(s.T(), metadataCertManagerIssuer.Revision, 1)
	assert.NotNil(s.T(), metadataCertManagerIssuer.Values)
	assert.Equal(s.T(), metadataCertManagerIssuer.Version, "0.1.0")


	config, err := awsHelper.NewK8SClientConfig(cluster)
	assert.NoError(s.T(), err)
	assert.NotNil(s.T(), config)

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(fmt.Errorf("failed to create dynamic client: %v", err))
	}

	verifyClusterIssuerStatus(s.T(), dynamicClient, "letsencrypt-prod")
	verifyClusterIssuerStatus(s.T(), dynamicClient, "letsencrypt-staging")

	certificate := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "Certificate",
			"metadata": map[string]interface{}{
				"name":      certName,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"secretName": certName,
				"commonName": domainName,
				"dnsNames": []interface{}{
					domainName,
					fmt.Sprintf("www.%s", domainName),
				},
				"issuerRef": map[string]interface{}{
					"name": "letsencrypt-prod",
					"kind": "ClusterIssuer",
				},
			},
		},
	}

	// Create the Certificate resource in the specified namespace
	certGVR := schema.GroupVersionResource{
		Group:    "cert-manager.io",
		Version:  "v1",
		Resource: "certificates",
	}

	_, err = dynamicClient.Resource(certGVR).Namespace(namespace).Create(context.Background(), certificate, metav1.CreateOptions{})
	assert.NoError(s.T(), err)

	time.Sleep(2 * time.Minute) // Wait for the certificate to be ready

	certificateName := certName
	certificateGVR := schema.GroupVersionResource{
		Group:    "cert-manager.io",
		Version:  "v1",
		Resource: "certificates",
	}

	// Wait for the Certificate status to be ready
	for {
		cert, err := dynamicClient.Resource(certificateGVR).Namespace(namespace).Get(context.Background(), certificateName, metav1.GetOptions{})
		assert.NoError(s.T(), err)

		conditions, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
		assert.NoError(s.T(), err, "error retrieving conditions from status")
		assert.True(s.T(), found, "conditions field not found in status")

		// Check if the certificate is ready
		isReady := false
		for _, condition := range conditions {
			conditionMap := condition.(map[string]interface{})
			if conditionMap["type"] == "Ready" && conditionMap["status"] == "True" {
				isReady = true
				break
			}
		}

		if isReady {
			break
		}

		time.Sleep(10 * time.Second) // Wait before checking again
	}

	s.DriftTest(component, stack, &inputs)
}

func (s *ComponentSuite) TestEnabledFlag() {
	const component = "eks/cert-manager/disabled"
	const stack = "default-test"
	s.VerifyEnabledFlag(component, stack, nil)
}

func (s *ComponentSuite) SetupSuite() {
	s.TestSuite.InitConfig()
	s.TestSuite.Config.ComponentDestDir = "components/terraform/eks/cert-manager"
	s.TestSuite.SetupSuite()
}

func TestRunSuite(t *testing.T) {
	suite := new(ComponentSuite)
	suite.AddDependency(t, "vpc", "default-test", nil)
	suite.AddDependency(t, "eks/cluster", "default-test", nil)

	subdomain := strings.ToLower(random.UniqueId())
	inputs := map[string]interface{}{
		"zone_config": []map[string]interface{}{
			{
				"subdomain": subdomain,
				"zone_name": "components.cptest.test-automation.app",
			},
		},
	}
	suite.AddDependency(t, "dns-delegated", "default-test", &inputs)
	helper.Run(t, suite)
}


func verifyClusterIssuerStatus(t *testing.T, dynamicClient dynamic.Interface, issuerName string) {
	clusterIssuerGVR := schema.GroupVersionResource{
		Group:    "cert-manager.io",
		Version:  "v1",
		Resource: "clusterissuers",
	}

	// Create the DNSEndpoint resource in the "default" namespace
	letsencryptProd, err := dynamicClient.Resource(clusterIssuerGVR).Get(context.Background(), issuerName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, letsencryptProd)

	conditions, found, err := unstructured.NestedSlice(letsencryptProd.Object, "status", "conditions")
	assert.NoError(t, err, "error retrieving conditions from status")
	assert.True(t, found, "conditions field not found in status")
	assert.NotEmpty(t, conditions, "conditions slice is empty")

	// Extract the first condition from the slice.
	firstCondition, ok := conditions[0].(map[string]interface{})
	assert.True(t, ok, "first condition is not a map[string]interface{}")

	// Use the unstructured helper to retrieve the 'status' field from the condition.
	conditionStatus, found, err := unstructured.NestedString(firstCondition, "status")
	assert.NoError(t, err, "error retrieving status from first condition")
	assert.True(t, found, "status field not found in first condition")

	// Assert that the condition status is "True".
	assert.Equal(t, "True", conditionStatus)
}