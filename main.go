package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func main() {
	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/mutate", handleMutate)

	log.Println("Starting kube-mutator webhook server on port 8443...")
	log.Fatal(http.ListenAndServeTLS(
		":8443",
		"/etc/webhook/certs/tls.crt",
		"/etc/webhook/certs/tls.key",
		nil,
	))
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "kube-mutator webhook is running")
}

func handleMutate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("could not read request body: %v", err)
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}

	var admissionReviewReq admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReviewReq); err != nil {
		log.Printf("could not unmarshal admission review: %v", err)
		http.Error(w, "could not unmarshal admission review", http.StatusBadRequest)
		return
	}

	log.Printf("Received admission review for: %s/%s",
		admissionReviewReq.Request.Namespace,
		admissionReviewReq.Request.Name,
	)

	var deployment appsv1.Deployment
	if err := json.Unmarshal(admissionReviewReq.Request.Object.Raw, &deployment); err != nil {
		log.Printf("could not unmarshal deployment: %v", err)
		http.Error(w, "could not unmarshal deployment", http.StatusBadRequest)
		return
	}

	patches := mutateDeployment(deployment)

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		log.Printf("could not marshal patch: %v", err)
		http.Error(w, "could not marshal patch", http.StatusInternalServerError)
		return
	}

	patchType := admissionv1.PatchTypeJSONPatch
	admissionReviewResponse := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:       admissionReviewReq.Request.UID,
			Allowed:   true,
			PatchType: &patchType,
			Patch:     patchBytes,
		},
	}

	responseBytes, err := json.Marshal(admissionReviewResponse)
	if err != nil {
		log.Printf("could not marshal response: %v", err)
		http.Error(w, "could not marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(responseBytes)
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func mutateDeployment(deployment appsv1.Deployment) []patchOperation {
	var patches []patchOperation

	for i, container := range deployment.Spec.Template.Spec.Containers {
		if container.Resources.Limits == nil {
			log.Printf("Container %s has no resource limits, injecting...", container.Name)

			patches = append(patches, patchOperation{
				Op:   "add",
				Path: fmt.Sprintf("/spec/template/spec/containers/%d/resources", i),
				Value: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			})
		}
	}

	return patches
}
