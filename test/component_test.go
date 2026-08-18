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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
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

	assert.Equal(s.T(), metadataCertManager.AppVersion, "v1.21.1")
	assert.Equal(s.T(), metadataCertManager.Chart, "cert-manager")
	assert.NotNil(s.T(), metadataCertManager.FirstDeployed)
	assert.NotNil(s.T(), metadataCertManager.LastDeployed)
	assert.Equal(s.T(), metadataCertManager.Name, "cert-manager")
	assert.Equal(s.T(), metadataCertManager.Namespace, namespace)
	assert.NotEmpty(s.T(), metadataCertManager.Notes)
	assert.Equal(s.T(), metadataCertManager.Revision, 1)
	assert.NotNil(s.T(), metadataCertManager.Values)
	assert.Equal(s.T(), metadataCertManager.Version, "v1.21.1")


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
	assert.Equal(s.T(), metadataCertManagerIssuer.Version, "0.2.0")


	config, err := awsHelper.NewK8SClientConfig(cluster)
	assert.NoError(s.T(), err)
	assert.NotNil(s.T(), config)

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(fmt.Errorf("failed to create dynamic client: %v", err))
	}

	// Registered after the destroy defer so it runs first: the namespace cannot terminate
	// while ACME Orders/Challenges hold their finalizers, and only the cert-manager
	// controller can clear them, so they have to drain before the release is uninstalled.
	defer waitForACMEResourceCleanup(s.T(), dynamicClient, namespace)

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

	defer func() {
		err := dynamicClient.Resource(certGVR).Namespace(namespace).Delete(context.Background(), certName, metav1.DeleteOptions{})
		assert.NoError(s.T(), err)
	}()

	_, err = dynamicClient.Resource(certGVR).Namespace(namespace).Create(context.Background(), certificate, metav1.CreateOptions{})
	assert.NoError(s.T(), err)

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	informer := factory.ForResource(certGVR).Informer()

	stopChannel := make(chan struct{})

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			cert := newObj.(*unstructured.Unstructured)

			if cert.GetName() != certName {
				fmt.Printf("Certificate name is not 'test', it is '%s'\n", cert.GetName())
				return
			}
			conditions, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")

			if err != nil || !found {
				fmt.Println("Error retrieving conditions from status")
				return
			}

			// Check if the certificate is ready
			for _, condition := range conditions {
				conditionMap := condition.(map[string]interface{})
				if conditionMap["type"] == "Ready" && conditionMap["status"] == "True" {
					close(stopChannel) // Stop the informer if the certificate is ready
					return
				}
			}
		},
	})

	go informer.Run(stopChannel)

	select {
		case <-stopChannel:
			msg := "Certificate is ready"
			fmt.Println(msg)
		case <-time.After(5 * time.Minute):
			msg := "Certificate is not ready"
			assert.Fail(s.T(), msg)
	}

	// The basic fixture sets `ingress_shim_default_issuer_name: selfsigning-issuer`, so an
	// annotated Ingress that names no issuer must resolve to that default. The fixture points
	// at the self-signed ClusterIssuer rather than an ACME one on purpose: a shim-generated
	// ACME certificate would open a DNS-01 Order that outlives the check and blocks the
	// namespace from terminating during destroy.
	verifyIngressShimDefaultIssuer(s.T(), dynamicClient, namespace, fmt.Sprintf("ingress-%s", randomID), fmt.Sprintf("shim.%s", domainName), "selfsigning-issuer")

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


// verifyIngressShimDefaultIssuer creates an Ingress annotated for TLS but without a
// `cert-manager.io/cluster-issuer` annotation, and asserts that the Certificate
// ingress-shim generates for it points at the chart's configured default issuer.
func verifyIngressShimDefaultIssuer(t *testing.T, dynamicClient dynamic.Interface, namespace string, ingressName string, host string, issuerName string) {
	ingressGVR := schema.GroupVersionResource{
		Group:    "networking.k8s.io",
		Version:  "v1",
		Resource: "ingresses",
	}
	certGVR := schema.GroupVersionResource{
		Group:    "cert-manager.io",
		Version:  "v1",
		Resource: "certificates",
	}

	// ingress-shim names the generated Certificate after the TLS secret.
	certName := ingressName

	ingress := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "Ingress",
			"metadata": map[string]interface{}{
				"name":      ingressName,
				"namespace": namespace,
				"annotations": map[string]interface{}{
					"kubernetes.io/tls-acme": "true",
				},
			},
			"spec": map[string]interface{}{
				"tls": []interface{}{
					map[string]interface{}{
						"hosts":      []interface{}{host},
						"secretName": certName,
					},
				},
				"rules": []interface{}{
					map[string]interface{}{
						"host": host,
						"http": map[string]interface{}{
							"paths": []interface{}{
								map[string]interface{}{
									"path":     "/",
									"pathType": "Prefix",
									"backend": map[string]interface{}{
										"service": map[string]interface{}{
											"name": "placeholder",
											"port": map[string]interface{}{
												"number": int64(80),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Deleting the Ingress garbage-collects the Certificate it owns, but garbage collection is
	// asynchronous and races the Terraform destroy that follows. Delete the Certificate
	// explicitly too so the namespace is empty of cert-manager resources by the time the Helm
	// release goes away.
	defer func() {
		err := dynamicClient.Resource(ingressGVR).Namespace(namespace).Delete(context.Background(), ingressName, metav1.DeleteOptions{})
		assert.NoError(t, err)

		if err := dynamicClient.Resource(certGVR).Namespace(namespace).Delete(context.Background(), certName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			assert.NoError(t, err)
		}
	}()

	_, err := dynamicClient.Resource(ingressGVR).Namespace(namespace).Create(context.Background(), ingress, metav1.CreateOptions{})
	if !assert.NoError(t, err) {
		return
	}

	// ingress-shim reconciles the Ingress asynchronously, so poll for the Certificate.
	const pollInterval = 5 * time.Second
	const pollTimeout = 2 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()

	for {
		cert, getErr := dynamicClient.Resource(certGVR).Namespace(namespace).Get(ctx, certName, metav1.GetOptions{})
		if getErr == nil && cert != nil {
			name, found, nestedErr := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
			if nestedErr == nil && found {
				assert.Equal(t, issuerName, name)
				kind, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "kind")
				assert.Equal(t, "ClusterIssuer", kind)
				return
			}
		}

		select {
		case <-ctx.Done():
			assert.Fail(t, fmt.Sprintf("ingress-shim did not generate Certificate %q with a default issuerRef within %s (last Get error: %v)", certName, pollTimeout, getErr))
			return
		case <-time.After(pollInterval):
		}
	}
}

// waitForACMEResourceCleanup blocks until no cert-manager ACME Orders or Challenges remain
// in the namespace. Both carry `finalizer.acme.cert-manager.io`, and only the cert-manager
// controller can clear it — so any that survive the Helm uninstall leave the namespace stuck
// in Terminating and `kubernetes_namespace` deletion fails with "context deadline exceeded"
// after the provider's five-minute delete timeout. Draining them while the controller is
// still running keeps the Terraform destroy deterministic.
func waitForACMEResourceCleanup(t *testing.T, dynamicClient dynamic.Interface, namespace string) {
	acmeGVRs := []schema.GroupVersionResource{
		{Group: "acme.cert-manager.io", Version: "v1", Resource: "orders"},
		{Group: "acme.cert-manager.io", Version: "v1", Resource: "challenges"},
	}

	const pollInterval = 5 * time.Second
	const pollTimeout = 3 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()

	for {
		remaining := []string{}
		for _, gvr := range acmeGVRs {
			list, err := dynamicClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					// The CRD is already gone, so nothing of this kind can be left behind.
					continue
				}
				// Anything else is inconclusive; keep polling rather than declaring the
				// namespace clean.
				remaining = append(remaining, fmt.Sprintf("%s (list error: %v)", gvr.Resource, err))
				continue
			}
			for _, item := range list.Items {
				remaining = append(remaining, fmt.Sprintf("%s/%s", gvr.Resource, item.GetName()))
			}
		}

		if len(remaining) == 0 {
			return
		}

		select {
		case <-ctx.Done():
			assert.Fail(t, fmt.Sprintf("cert-manager ACME resources still present in namespace %q after %s: %v; their finalizers will block the namespace from terminating once the Helm release is removed", namespace, pollTimeout, remaining))
			return
		case <-time.After(pollInterval):
		}
	}
}

func verifyClusterIssuerStatus(t *testing.T, dynamicClient dynamic.Interface, issuerName string) {
	clusterIssuerGVR := schema.GroupVersionResource{
		Group:    "cert-manager.io",
		Version:  "v1",
		Resource: "clusterissuers",
	}

	// The controller populates .status.conditions asynchronously after the
	// ClusterIssuer is created (ACME issuers only become Ready after account
	// registration), so poll for the Ready condition instead of asserting on
	// a single immediate read. The deadline context bounds both the polling
	// loop and each individual Kubernetes request.
	const pollInterval = 5 * time.Second
	const pollTimeout = 2 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()

	for {
		clusterIssuer, err := dynamicClient.Resource(clusterIssuerGVR).Get(ctx, issuerName, metav1.GetOptions{})
		if err == nil && clusterIssuer != nil {
			conditions, found, nestedErr := unstructured.NestedSlice(clusterIssuer.Object, "status", "conditions")
			if nestedErr == nil && found {
				// Same readiness pattern as the Certificate check in TestBasic.
				for _, condition := range conditions {
					conditionMap, ok := condition.(map[string]interface{})
					if !ok {
						continue
					}
					if conditionMap["type"] == "Ready" && conditionMap["status"] == "True" {
						return
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			assert.Fail(t, fmt.Sprintf("ClusterIssuer %q did not report a Ready=True condition within %s (last Get error: %v)", issuerName, pollTimeout, err))
			return
		case <-time.After(pollInterval):
		}
	}
}