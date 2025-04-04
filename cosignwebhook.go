package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/golang/glog"
	v1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"

	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	ociremote "github.com/sigstore/cosign/pkg/oci/remote"
	"github.com/sigstore/cosign/v2/pkg/cosign"

	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
)

const (
	admissionApi  = "admission.k8s.io/v1"
	admissionKind = "AdmissionReview"
	cosignEnvVar  = "COSIGNPUBKEY"
)

// CosignServerHandler listen to admission requests and serve responses
// build certs here: https://raw.githubusercontent.com/openshift/external-dns-operator/fb77a3c547a09cd638d4e05a7b8cb81094ff2476/hack/generate-certs.sh
// generate-certs.sh --service cosignwebhook --webhook cosignwebhook --namespace cosignwebhook --secret cosignwebhook
type CosignServerHandler struct{}

// create restClient for get secrets and create events
func restClient() (*kubernetes.Clientset, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		glog.Errorf("error init in-cluster config: %v", err)
		return nil, err
	}
	k8sclientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		glog.Errorf("error creating k8sclientset: %v", err)
		return nil, err
	}
	return k8sclientset, err
}

func recordEvent(pod *corev1.Pod, k8sclientset *kubernetes.Clientset) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: k8sclientset.CoreV1().Events("")})
	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "Cosignwebhook"})
	eventRecorder.Eventf(pod, corev1.EventTypeNormal, "Cosignwebhook", "Cosign image verified")
	eventBroadcaster.Shutdown()
}

// get pod object from admission request
func getPod(byte []byte) (*corev1.Pod, *v1.AdmissionReview, error) {
	arRequest := v1.AdmissionReview{}
	if err := json.Unmarshal(byte, &arRequest); err != nil {
		glog.Error("incorrect body")
		return nil, nil, err
	}
	if arRequest.Request == nil {
		glog.Error("AdmissionReview request not found")
		return nil, nil, fmt.Errorf("admissionreview request not found")
	}
	raw := arRequest.Request.Object.Raw
	pod := corev1.Pod{}
	if err := json.Unmarshal(raw, &pod); err != nil {
		glog.Error("Error deserializing pod")
		return nil, nil, err
	}
	return &pod, &arRequest, nil
}

// get pubKey from Env
func getEnv(pod *corev1.Pod) (string, error) {
	for i := 0; i < len(pod.Spec.Containers[0].Env); i++ {
		value := pod.Spec.Containers[0].Env[i].Value
		if pod.Spec.Containers[0].Env[i].Name == cosignEnvVar {
			return value, nil
		}
	}
	return "", fmt.Errorf("no env var found %s/%s", pod.Namespace, pod.Name)
}

// get pubKey Secrets value by given name with kubernetes in-cluster client
func getSecret(namespace string, name string) (string, error) {
	clientset, err := restClient()
	if err != nil {
		glog.Errorf("Can't init rest client for secret: %v", err)
		return "", err
	}
	secret, err := clientset.CoreV1().Secrets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Can't get secret %s/%s from kubernetes: %v", namespace, name, err)
		return "", err
	}
	value := secret.Data[cosignEnvVar]
	if value == nil {
		glog.Errorf("Secret value empty for %s/%s", namespace, name)
		return "", nil
	}
	/*
		decodedValue, err := base64.StdEncoding.DecodeString(string(value))
		if err != nil {
			glog.Error("Can't decode value ", err)
			return "", err
		}
	*/
	return string(value), nil
}

func (cs *CosignServerHandler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (cs *CosignServerHandler) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	// Url path of metrics
	if r.URL.Path == "/metrics" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Url path of admission
	if r.URL.Path != "/validate" {
		glog.Error("no validate")
		http.Error(w, "no validate", http.StatusBadRequest)
		return
	}

	if len(body) == 0 {
		glog.Error("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// count each request for prometheus metric
	opsProcessed.Inc()

	pod, arRequest, err := getPod(body)
	if err != nil {
		glog.Errorf("Error getPod in %s/%s: %v", pod.Namespace, pod.Name, err)
		http.Error(w, "incorrect body", http.StatusBadRequest)
		return
	}

	pubKey, err := getEnv(pod)
	if err != nil {
		glog.Warningf("Could not get public key from environment variable in %s/%s: %v. Trying to get public key from secret", pod.Namespace, pod.Name, err)
	}

	if len(pubKey) == 0 {
		pubKey, err = getSecret(pod.Namespace, "cosignwebhook")
		if err != nil {
			glog.Warningf("Could not get public key from secret in %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}

	if len(pubKey) == 0 {
		glog.Errorf("No public key set in %s/%s", pod.Namespace, pod.Name)
		// return OK if no key is set, so user don't want a verification
		// otherwise set failurePolicy: Skip in ValidatingWebhookConfiguration
		resp, err := json.Marshal(admissionResponse(200, true, "Success", "Cosign image skipped", arRequest))
		if err != nil {
			glog.Errorf("Can't encode response: %v", err)
			http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		}
		if _, err := w.Write(resp); err != nil {
			glog.Errorf("Can't write response: %v", err)
			http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
		}
		return
	}
	glog.Info("Successfully got public key")

	// Lookup image name of first container
	image := pod.Spec.Containers[0].Image
	refImage, err := name.ParseReference(image)
	if err != nil {
		glog.Errorf("Error ParseRef image: %v", err)
		resp, err := json.Marshal(admissionResponse(403, false, "Failure", "Cosign ParseRef image failed", arRequest))
		if err != nil {
			glog.Errorf("Can't encode response %s/%s: %v", pod.Namespace, pod.Name, err)
			http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		}
		if _, err := w.Write(resp); err != nil {
			glog.Errorf("Can't write response: %v", err)
			http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
		}
		return
	}

	// Lookup imagePullSecrets for reviewed POD
	imagePullSecrets := make([]string, 0, len(pod.Spec.ImagePullSecrets))
	for _, s := range pod.Spec.ImagePullSecrets {
		imagePullSecrets = append(imagePullSecrets, s.Name)
	}
	opt := k8schain.Options{
		Namespace:          pod.Namespace,
		ServiceAccountName: pod.Spec.ServiceAccountName,
		ImagePullSecrets:   imagePullSecrets,
	}

	publicKey, err := cryptoutils.UnmarshalPEMToPublicKey([]byte(pubKey))
	if err != nil {
		glog.Errorf("Error UnmarshalPEMToPublicKey %s/%s: %v", pod.Namespace, pod.Name, err)
		resp, err := json.Marshal(admissionResponse(403, false, "Failure", "Cosign UnmarshalPEMToPublicKey failed", arRequest))
		if err != nil {
			glog.Errorf("Can't encode response %s/%s: %v", pod.Namespace, pod.Name, err)
			http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		}
		if _, err := w.Write(resp); err != nil {
			glog.Errorf("Can't write response: %v", err)
			http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
		}
		return
	}

	cosignLoadKey, err := signature.LoadECDSAVerifier(publicKey.(*ecdsa.PublicKey), crypto.SHA256)
	if err != nil {
		glog.Errorf("Error LoadECDSAVerifier %s/%s: %v", pod.Namespace, pod.Name, err)
		resp, err := json.Marshal(admissionResponse(403, false, "Failure", "Cosign key encoding failed", arRequest))
		if err != nil {
			glog.Errorf("Can't encode response %s/%s: %v", pod.Namespace, pod.Name, err)
			http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		}
		if _, err := w.Write(resp); err != nil {
			glog.Errorf("Can't write response %s/%s: %v", pod.Namespace, pod.Name, err)
			http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
		}
		return
	}

	kc, err := k8schain.NewInCluster(context.Background(), opt)
	if err != nil {
		glog.Errorf("Error k8schain %s/%s: %v", pod.Namespace, pod.Name, err)
		resp, err := json.Marshal(admissionResponse(403, false, "Failure", "Cosign UnmarshalPEMToPublicKey failed", arRequest))
		if err != nil {
			glog.Errorf("Can't encode response %s/%s: %v", pod.Namespace, pod.Name, err)
			http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		}
		if _, err := w.Write(resp); err != nil {
			glog.Errorf("Can't write response: %v", err)
			http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
		}
		return
	}

	remoteOpts := []ociremote.Option{ociremote.WithRemoteOptions(remote.WithAuthFromKeychain(kc))}
	_, _, err = cosign.VerifyImageSignatures(
		context.Background(),
		refImage,
		&cosign.CheckOpts{
			RegistryClientOpts: remoteOpts,
			SigVerifier:        cosignLoadKey,
			// add settings for cosign 2.0
			// IgnoreSCT:      true,
			// SkipTlogVerify: true,
		})

	// this is always false,
	// glog.Info("Resp bundleVerified: ", bundleVerified)

	// Verify Image failed, needs to reject pod start
	if err != nil {
		glog.Errorf("Error VerifyImageSignatures %s/%s: %v", pod.Namespace, pod.Name, err)
		resp, err := json.Marshal(admissionResponse(403, false, "Failure", "Cosign image verification failed", arRequest))
		if err != nil {
			glog.Errorf("Can't encode response: %v", err)
			http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		}
		if _, err := w.Write(resp); err != nil {
			glog.Errorf("Can't write response: %v", err)
			http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
		}
	} else {
		// count successful verifies for prometheus metric
		verifiedProcessed.Inc()
		glog.Info("Image successful verified: ", pod.Namespace, "/", pod.Name)
		resp, err := json.Marshal(admissionResponse(200, true, "Success", "Cosign image verified", arRequest))
		// Verify Image successful, needs to allow pod start
		if err != nil {
			glog.Errorf("Can't encode response: %v", err)
			http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		}
		if _, err := w.Write(resp); err != nil {
			glog.Errorf("Can't write response: %v", err)
			http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
		}

		clientset, err := restClient()
		if err != nil {
			glog.Errorf("Can't init rest client for event recorder: %v", err)
		} else {
			recordEvent(pod, clientset)
		}
	}
}

// Template for AdmissionReview
func admissionResponse(admissionCode int32, admissionPermissions bool, admissionStatus string, admissionMessage string, ar *v1.AdmissionReview) v1.AdmissionReview {
	return v1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       admissionKind,
			APIVersion: admissionApi,
		},
		Response: &v1.AdmissionResponse{
			Allowed: admissionPermissions,
			UID:     ar.Request.UID,
			Result: &metav1.Status{
				Status:  admissionStatus,
				Message: admissionMessage,
				Code:    admissionCode,
			},
		},
	}
}
