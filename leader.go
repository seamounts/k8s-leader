package leader

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/labstack/gommon/log"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// PodNameEnvVar is the constant for env variable POD_NAME
	// which is the name of the current pod.
	PodNameEnvVar = "POD_NAME"

	// maxBackoffInterval defines the maximum amount of time to wait between
	// attempts to become the leader.
	maxBackoffInterval = time.Second * 16
)

// Become ensures that the current pod is the leader within its namespace. If
// run outside a cluster, it will skip leader election and return nil. It
// continuously tries to create a ConfigMap with the provided name and the
// current pod set as the owner reference. Only one can exist at a time with
// the same name, so the pod that successfully creates the ConfigMap is the
// leader. Upon termination of that pod, the garbage collector will delete the
// ConfigMap, enabling a different pod to become the leader.
func Become(lockName string) error {
	log.Info("Trying to become the leader.")

	ns, err := getNamespace()
	if err != nil {
		return err
	}

	conf, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	client := kubernetes.NewForConfigOrDie(conf)

	owner, err := myOwnerRef(client, ns)
	if err != nil {
		return err
	}

	existing, err := client.CoreV1().ConfigMaps(ns).Get(lockName, metav1.GetOptions{})

	switch {
	case err == nil:
		for _, existingOwner := range existing.GetOwnerReferences() {
			if existingOwner.Name == owner.Name {
				log.Info("Found existing lock with my name. I was likely restarted.")
				log.Info("Continuing as the leader.")
				return nil
			}

			log.Info("Found existing lock", "LockOwner", existingOwner.Name)
		}
	case apierrors.IsNotFound(err):
		log.Info("No pre-existing lock was found.")
	default:
		log.Error(err, "Unknown error trying to get ConfigMap")
		return err
	}

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            lockName,
			Namespace:       ns,
			OwnerReferences: []metav1.OwnerReference{*owner},
		},
	}

	// try to create a lock
	backoff := time.Second
	for {
		_, err := client.CoreV1().ConfigMaps(ns).Create(cm)
		switch {
		case err == nil:
			log.Info("Became the leader.")
			return nil
		case apierrors.IsAlreadyExists(err):
			existingOwners := existing.GetOwnerReferences()
			switch {
			case len(existingOwners) != 1:
				log.Info("Leader lock configmap must have exactly one owner reference.", "ConfigMap", existing)

			case existingOwners[0].Kind != "Pod":
				log.Info("Leader lock configmap owner reference must be a pod.", "OwnerReference", existingOwners[0])

			default:
				leaderPod, err := client.CoreV1().Pods(ns).Get(existingOwners[0].Name, metav1.GetOptions{})
				switch {
				case apierrors.IsNotFound(err):
					log.Info("Leader pod has been deleted, waiting for garbage collection do remove the lock.")
				case err != nil:
					return err
				case isPodEvicted(leaderPod) && leaderPod.GetDeletionTimestamp() == nil:
					log.Info("pod with leader lock has been evicted.", "leader", leaderPod.Name)
					log.Info("Deleting evicted leader.")
					err := client.CoreV1().Pods(ns).Delete(leaderPod.Name, &metav1.DeleteOptions{})
					if err != nil {
						log.Error(err, "Leader pod could not be deleted.")
					}
				default:
					log.Info("Not the leader. Waiting.")
				}
			}

			select {
			case <-time.After(wait.Jitter(backoff, .2)):
				if backoff < maxBackoffInterval {
					backoff *= 2
				}
				continue
			}

		default:
			log.Error(err, "Unknown error creating ConfigMap")
			return err
		}
	}
}

func myOwnerRef(client *kubernetes.Clientset, ns string) (*metav1.OwnerReference, error) {
	myPod, err := getMyPod(client, ns)
	if err != nil {
		return nil, err
	}

	owner := &metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       myPod.ObjectMeta.Name,
		UID:        myPod.ObjectMeta.UID,
	}

	return owner, nil
}

func isPodEvicted(pod *v1.Pod) bool {
	podFailed := pod.Status.Phase == v1.PodFailed
	podEvicted := pod.Status.Reason == "Evicted"
	return podFailed && podEvicted
}

func getMyPod(client *kubernetes.Clientset, ns string) (*v1.Pod, error) {
	podName := os.Getenv(PodNameEnvVar)
	if podName == "" {
		return nil, fmt.Errorf("required env %s not set, please configure downward API", PodNameEnvVar)
	}

	pod, err := client.CoreV1().Pods(ns).Get(podName, metav1.GetOptions{})
	if err != nil {
		log.Errorf("Failed to get Pod, Pod.Namespace %s, Pod.Name %s", ns, podName)
		return nil, err
	}

	return pod, err
}

func getNamespace() (string, error) {
	nsBytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("namespace not found for current environment")
		}
		return "", err
	}
	ns := strings.TrimSpace(string(nsBytes))

	return ns, nil
}
