package registry

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/covexo/devspace/pkg/devspace/config/v1"

	"github.com/covexo/devspace/pkg/util/log"
	"github.com/foomo/htpasswd"
	"k8s.io/client-go/kubernetes"

	"github.com/covexo/devspace/pkg/devspace/clients/helm"
	"github.com/covexo/devspace/pkg/devspace/config/configutil"
	"github.com/covexo/yamlq"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const registryAuthSecretNamePrefix = "devspace-registry-auth-"
const registryPort = 5000

// CreatePullSecret creates an image pull secret for a registry
func CreatePullSecret(kubectl *kubernetes.Clientset, namespace, registryURL, username, passwordOrToken, email string) error {
	pullSecretName := GetRegistryAuthSecretName(registryURL)

	if registryURL == "hub.docker.com" || registryURL == "" {
		registryURL = "https://index.docker.io/v1/"
	}
	authToken := passwordOrToken

	if username != "" {
		authToken = username + ":" + authToken
	}
	registryAuthEncoded := base64.StdEncoding.EncodeToString([]byte(authToken))
	pullSecretDataValue := []byte(`{
			"auths": {
				"` + registryURL + `": {
					"auth": "` + registryAuthEncoded + `",
					"email": "` + email + `"
				}
			}
		}`)

	pullSecretData := map[string][]byte{}
	pullSecretDataKey := k8sv1.DockerConfigJsonKey
	pullSecretData[pullSecretDataKey] = pullSecretDataValue

	registryPullSecret := &k8sv1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: pullSecretName,
		},
		Data: pullSecretData,
		Type: k8sv1.SecretTypeDockerConfigJson,
	}
	_, err := kubectl.Core().Secrets(namespace).Get(pullSecretName, metav1.GetOptions{})

	if err != nil {
		_, err = kubectl.Core().Secrets(namespace).Create(registryPullSecret)
	} else {
		_, err = kubectl.Core().Secrets(namespace).Update(registryPullSecret)
	}

	if err != nil {
		return fmt.Errorf("Unable to update image pull secret: %s", err.Error())
	}
	return nil
}

// GetRegistryAuthSecretName returns the name of the image pull secret for a registry
func GetRegistryAuthSecretName(registryURL string) string {
	registryHash := md5.Sum([]byte(registryURL))

	return registryAuthSecretNamePrefix + hex.EncodeToString(registryHash[:])
}

// InitInternalRegistry deploys and starts a new docker registry if necessary
func InitInternalRegistry(kubectl *kubernetes.Clientset, helm *helm.HelmClientWrapper, internalRegistry *v1.InternalRegistry, registryConfig *v1.RegistryConfig) error {
	registryReleaseName := *internalRegistry.Release.Name
	registryReleaseDeploymentName := registryReleaseName + "-docker-registry"
	registryReleaseNamespace := *internalRegistry.Release.Namespace
	registryReleaseValues := internalRegistry.Release.Values

	// Check if registry already exists
	registryDeployment, err := kubectl.ExtensionsV1beta1().Deployments(registryReleaseNamespace).Get(registryReleaseDeploymentName, metav1.GetOptions{})
	if err != nil {
		// Check if registry namespace exists
		_, err := kubectl.CoreV1().Namespaces().Get(registryReleaseNamespace, metav1.GetOptions{})
		if err != nil {
			// Create registry namespace
			_, err = kubectl.CoreV1().Namespaces().Create(&k8sv1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: registryReleaseNamespace,
				},
			})

			if err != nil {
				return err
			}
		}

		_, err = helm.InstallChartByName(registryReleaseName, registryReleaseNamespace, "stable/docker-registry", "", registryReleaseValues)
		if err != nil {
			return fmt.Errorf("Unable to initialize docker registry: %s", err.Error())
		}

		if registryConfig != nil && registryConfig.Auth != nil {
			registryAuth := registryConfig.Auth
			htpasswdSecretName := registryReleaseName + "-docker-registry-secret"
			htpasswdSecret, err := kubectl.Core().Secrets(registryReleaseNamespace).Get(htpasswdSecretName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("Unable to retrieve secret for docker registry: %s", err.Error())
			}

			if htpasswdSecret == nil || htpasswdSecret.Data == nil {
				htpasswdSecret = &k8sv1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: htpasswdSecretName,
					},
					Data: map[string][]byte{},
				}
			}

			oldHtpasswdData := htpasswdSecret.Data["htpasswd"]
			newHtpasswdData := htpasswd.HashedPasswords{}

			if len(oldHtpasswdData) != 0 {
				oldHtpasswdDataBytes := []byte(oldHtpasswdData)
				newHtpasswdData, _ = htpasswd.ParseHtpasswd(oldHtpasswdDataBytes)
			}

			err = newHtpasswdData.SetPassword(*registryAuth.Username, *registryAuth.Password, htpasswd.HashBCrypt)
			if err != nil {
				return fmt.Errorf("Unable to set password in htpasswd: %s", err.Error())
			}

			newHtpasswdDataBytes := newHtpasswdData.Bytes()

			htpasswdSecret.Data["htpasswd"] = newHtpasswdDataBytes

			_, err = kubectl.Core().Secrets(registryReleaseNamespace).Get(htpasswdSecretName, metav1.GetOptions{})
			if err != nil {
				_, err = kubectl.Core().Secrets(registryReleaseNamespace).Create(htpasswdSecret)
			} else {
				_, err = kubectl.Core().Secrets(registryReleaseNamespace).Update(htpasswdSecret)
			}
		}

		if err != nil {
			return fmt.Errorf("Unable to update htpasswd secret: %s", err.Error())
		}

		registryServiceName := registryReleaseName + "-docker-registry"
		serviceHostname := ""
		maxServiceWaiting := 60 * time.Second
		serviceWaitingInterval := 3 * time.Second

		for true {
			registryService, err := kubectl.Core().Services(registryReleaseNamespace).Get(registryServiceName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			if len(registryService.Spec.ClusterIP) > 0 {
				serviceHostname = registryService.Spec.ClusterIP + ":" + strconv.Itoa(registryPort)
				break
			}

			time.Sleep(serviceWaitingInterval)
			maxServiceWaiting = maxServiceWaiting - serviceWaitingInterval

			if maxServiceWaiting <= 0 {
				return errors.New("Timeout waiting for registry service to start")
			}
		}

		ingressHostname := ""
		if registryReleaseValues != nil {
			registryValues := yamlq.NewQuery(*registryReleaseValues)
			isIngressEnabled, _ := registryValues.Bool("ingress", "enabled")

			if isIngressEnabled {
				firstIngressHostname, _ := registryValues.String("ingress", "hosts", "0")

				if len(firstIngressHostname) > 0 {
					ingressHostname = firstIngressHostname
				}
			}
		}

		if len(ingressHostname) == 0 {
			registryConfig.URL = configutil.String(serviceHostname)
			registryConfig.Insecure = configutil.Bool(true)
		} else {
			registryConfig.URL = configutil.String(ingressHostname)
			registryConfig.Insecure = configutil.Bool(false)
		}
	}

	// Wait for registry if it is not ready yet
	if registryDeployment == nil || registryDeployment.Status.Replicas == 0 || registryDeployment.Status.ReadyReplicas != registryDeployment.Status.Replicas {
		// Wait till registry is started
		err = waitForRegistry(registryReleaseNamespace, registryReleaseDeploymentName, kubectl)
		if err != nil {
			return err
		}
	}

	return nil
}

func waitForRegistry(registryNamespace, registryReleaseDeploymentName string, client *kubernetes.Clientset) error {
	registryWaitingTime := 2 * 60 * time.Second
	registryCheckInverval := 5 * time.Second

	log.StartWait("Waiting for internal registry to start")
	defer log.StopWait()

	for registryWaitingTime > 0 {
		registryDeployment, err := client.ExtensionsV1beta1().Deployments(registryNamespace).Get(registryReleaseDeploymentName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if registryDeployment.Status.ReadyReplicas == registryDeployment.Status.Replicas {
			return nil
		}

		time.Sleep(registryCheckInverval)
		registryWaitingTime = registryWaitingTime - registryCheckInverval
	}

	return errors.New("Internal registry start waiting time timed out")
}

//GetImageURL returns the image (optional with tag)
func GetImageURL(imageConfig *v1.ImageConfig, includingLatestTag bool) string {
	registryConfig, registryConfErr := GetRegistryConfig(imageConfig)

	if registryConfErr != nil {
		log.Fatal(registryConfErr)
	}
	image := *imageConfig.Name
	registryURL := *registryConfig.URL

	if registryURL != "" && registryURL != "hub.docker.com" {
		image = registryURL + "/" + image
	}

	if includingLatestTag {
		image = image + ":" + *imageConfig.Tag
	}
	return image
}

// GetRegistryConfig returns the registry config for an image or an error if the registry is not defined
func GetRegistryConfig(imageConfig *v1.ImageConfig) (*v1.RegistryConfig, error) {
	config := configutil.GetConfig(false)
	registryName := *imageConfig.Registry
	registryMap := *config.Registries
	registryConfig, registryFound := registryMap[registryName]

	if !registryFound {
		return nil, errors.New("Unable to find registry: " + registryName)
	}
	return registryConfig, nil
}
