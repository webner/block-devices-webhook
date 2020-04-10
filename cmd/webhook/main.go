package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"

	admiv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"

	"gomodules.xyz/jsonpatch/v3"
)

const (
	secretName = "webhook-cert"
	certFile   = "/certs/tls.crt"
	keyFile    = "/certs/tls.key"

	jsonContentType = `application/json`

	blockDeviceResource = "cloudflight.io/block-devices"

	provisionerEnv = "PROVISIONER_REGEX"
	provisionerKey = "volume.beta.kubernetes.io/storage-provisioner"
)

var (
	deserializer = serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()
	client       *kubernetes.Clientset
	provRegex    *regexp.Regexp
)

func main() {
	var err error
	provRegex, err = regexp.Compile(getEnv(provisionerKey, "cinder|vsphere"))
	if err != nil {
		log.Fatalf("Error compiling regex: %v", err)
	}

	config, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		log.Fatalf("Error building Kubernetes config: %v", err)
	}

	client, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error building Kubernetes client: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", mutateFunc)
	server := &http.Server{
		Addr:    ":10250",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServeTLS(certFile, keyFile))
}

func mutateFunc(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		klog.Errorf("invalid method %s, only POST requests are allowed", r.Method)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		klog.Errorf("could not read request body: %v", err)
		return
	}
	defer r.Body.Close()

	if contentType := r.Header.Get("Content-Type"); contentType != jsonContentType {
		w.WriteHeader(http.StatusBadRequest)
		klog.Errorf("unsupported content type %s, only %s is supported", contentType, jsonContentType)
		return
	}

	var admissionReviewReq admiv1beta1.AdmissionReview
	if _, _, err := deserializer.Decode(body, nil, &admissionReviewReq); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		klog.Errorf("could not deserialize request: %v", err)
		return
	} else if admissionReviewReq.Request == nil {
		w.WriteHeader(http.StatusBadRequest)
		klog.Errorf("malformed admission review: request is nil")
		return
	}

	var admissionReviewResp admiv1beta1.AdmissionReview
	resp, err := mutate(admissionReviewReq.Request)
	if err != nil {
		klog.Errorf("Failed to mutate: %v", err)
		admissionReviewResp = admiv1beta1.AdmissionReview{
			Response: &admiv1beta1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
				Allowed: true,
			},
		}
	} else {
		admissionReviewResp = admiv1beta1.AdmissionReview{
			Response: resp,
		}
	}
	admissionReviewResp.Response.UID = admissionReviewReq.Request.UID

	encoder := json.NewEncoder(w)
	err = encoder.Encode(&admissionReviewResp)
	if err != nil {
		klog.Errorf("failed to encode the response: %v", err)
		return
	}
}

func mutate(req *admiv1beta1.AdmissionRequest) (*admiv1beta1.AdmissionResponse, error) {
	var pod corev1.Pod

	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return nil, fmt.Errorf("failed to decode raw object: %w", err)
	}

	resp := &admiv1beta1.AdmissionResponse{Allowed: true}

	pvcs := int64(0)

	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {

			claim, err := client.CoreV1().PersistentVolumeClaims(req.Namespace).Get(v.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("Warning failed to get PVC, will assume blockstorage: %v", err)
				pvcs++
			} else {
				if prov, ok := claim.Annotations[provisionerKey]; ok {
					if provRegex.FindStringIndex(string(prov)) != nil {
						pvcs++
					}
				}
			}
		}
	}

	if pvcs > 0 {
		klog.Infof("Adding resource limit of %v block-devices to pod %v/%v", pvcs, req.Namespace, pod.GenerateName)
	}

	for i, container := range pod.Spec.Containers {
		// Find out if there is already an environment variable defined where we want to add one
		blockdevices := int64(0)

		if i == 0 {
			blockdevices = pvcs
		}

		if blockdevices == 0 {
			if container.Resources.Limits != nil {
				delete(container.Resources.Limits, blockDeviceResource)
			}
			if container.Resources.Requests != nil {
				delete(container.Resources.Requests, blockDeviceResource)
			}
		} else {
			if container.Resources.Limits == nil {
				container.Resources.Limits = corev1.ResourceList{}
			}
			if container.Resources.Requests == nil {
				container.Resources.Requests = corev1.ResourceList{}
			}
			container.Resources.Limits[blockDeviceResource] = *resource.NewQuantity(blockdevices, resource.DecimalSI)
			container.Resources.Requests[blockDeviceResource] = *resource.NewQuantity(blockdevices, resource.DecimalSI)
		}

		pod.Spec.Containers[i] = container
	}

	bytes, err := json.Marshal(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the mutated Pod object: %w", err)
	}
	patch, err := jsonpatch.CreatePatch(req.Object.Raw, bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to compute the JSON patch: %w", err)
	}
	resp.Patch, err = json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the JSON patch: %w", err)
	}
	return resp, nil
}

func getEnv(key string, defaultVal string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultVal
}
