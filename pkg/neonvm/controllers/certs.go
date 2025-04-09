package controllers

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"time"

	"github.com/cert-manager/cert-manager/pkg/apis/certmanager"
	certv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/cert-manager/cert-manager/pkg/util/pki"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

func (r *VMReconciler) reconcileCertificateSecret(ctx context.Context, vm *vmv1.VirtualMachine) (*corev1.Secret, error) {
	log := log.FromContext(ctx)

	certSecret := &corev1.Secret{}

	// Check if the TLS secret exists, if not start the creation routine.
	err := r.Get(ctx, types.NamespacedName{Name: vm.Status.TLSSecretName, Namespace: vm.Namespace}, certSecret)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to get vm TLS secret")
		return nil, err
	}

	certNotFound := false
	if err != nil /* not found */ {
		log.Info("VirtualMachine TLS secret not found", "Secret.Namespace", vm.Namespace, "Secret.Name", vm.Status.TLSSecretName)
		certNotFound = true
	} else {
		// check the certificate expiration
		certs, err := pki.DecodeX509CertificateChainBytes(certSecret.Data[corev1.TLSCertKey])
		if err != nil {
			log.Error(err, "Failed to parse VM certificate")
			return nil, err
		}
		renewAt := certs[0].NotAfter.Add(-vm.Spec.TLS.RenewBefore.Duration)

		// if not yet due for renewal
		if time.Now().Before(renewAt) {
			// just in case they were left around due to a transient issue.
			if err := r.cleanupTmpSecrets(ctx, vm); err != nil {
				return nil, err
			}

			return certSecret, nil
		}

		log.Info("VirtualMachine TLS secret is due for renewal", "Secret.Namespace", vm.Namespace, "Secret.Name", vm.Status.TLSSecretName)
	}

	// Check if the TLS private key temporary secret exists, if not create a new one
	tmpKeySecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-tmp", vm.Status.TLSSecretName), Namespace: vm.Namespace}, tmpKeySecret)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to get vm TLS secret")
		return nil, err
	} else if err != nil /* not found */ {
		tmpKeySecret, err = r.createTlsTmpSecret(ctx, vm)
		if err != nil {
			return nil, err
		}
	}

	key, err := pki.DecodePrivateKeyBytes(tmpKeySecret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		log.Error(err, "Failed to decode TLS private key")
		return nil, err
	}

	// Check if the TLS certificate already exists, if not create a new one
	certificateReq := &certv1.CertificateRequest{}
	err = r.Get(ctx, types.NamespacedName{Name: vm.Name, Namespace: vm.Namespace}, certificateReq)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to get vm CertificateRequest")
		return nil, err
	} else if err != nil /* not found */ {
		certificateReq, err = r.createCertificateRequest(ctx, vm, key)
		if err != nil {
			return nil, err
		}
	}

	if len(certificateReq.Status.Certificate) == 0 {
		// we cannot yet update the cert secret.
		// return it untouched.
		return certSecret, nil
	}

	// we have a certificate and the corresponding private key
	// create/update the proper TLS secret and delete the tmp secret
	if certNotFound {
		if err := r.createTlsSecret(ctx, vm, key, certificateReq); err != nil {
			return nil, err
		}
	} else if !reflect.DeepEqual(certificateReq.Status.Certificate, certSecret.Data[corev1.TLSCertKey]) {
		if err := r.updateTlsSecret(ctx, key, certificateReq, certSecret); err != nil {
			return nil, err
		}
	}

	// we made a lot of changes to state.
	// nil signals that we should re-schedule reconciliation with a refreshed state.
	return nil, nil
}

func (r *VMReconciler) cleanupTmpSecrets(ctx context.Context, vm *vmv1.VirtualMachine) error {
	log := log.FromContext(ctx)

	// Check if the TLS private key temporary secret exists, if so, delete it.
	tmpKeySecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-tmp", vm.Status.TLSSecretName), Namespace: vm.Namespace}, tmpKeySecret)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to get vm TLS secret")
		return err
	} else if err == nil /* found */ {
		if err := r.deleteTmpSecret(ctx, tmpKeySecret); err != nil {
			return err
		}
	}

	// Check if the TLS certificate already exists, if so, delete it.
	certificateReq := &certv1.CertificateRequest{}
	err = r.Get(ctx, types.NamespacedName{Name: vm.Name, Namespace: vm.Namespace}, certificateReq)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to get vm CertificateRequest")
		return err
	} else if err == nil /* found */ {
		if err := r.deleteCertRequest(ctx, certificateReq); err != nil {
			return err
		}
	}

	return nil
}

func (r *VMReconciler) createTlsTmpSecret(ctx context.Context, vm *vmv1.VirtualMachine) (*corev1.Secret, error) {
	log := log.FromContext(ctx)

	// create a new key for this VM
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Error(err, "Failed to generate TLS private key for VirtualMachine")
		return nil, err
	}

	// Define the secret
	tmpKeySecret, err := r.tmpKeySecretForVirtualMachine(vm, key)
	if err != nil {
		log.Error(err, "Failed to define new temporary TLS private key secret resource for VirtualMachine")
		return nil, err
	}

	if err = r.Create(ctx, tmpKeySecret); err != nil {
		log.Error(err, "Failed to create new temporary TLS private key secret", "Secret.Namespace", tmpKeySecret.Namespace, "Secret.Name", tmpKeySecret.Name)
		return nil, err
	}
	log.Info("Virtual Machine temporary TLS private key secret was created", "Secret.Namespace", tmpKeySecret.Namespace, "Secret.Name", tmpKeySecret.Name)

	return tmpKeySecret, nil
}

func (r *VMReconciler) createCertificateRequest(ctx context.Context, vm *vmv1.VirtualMachine, key crypto.Signer) (*certv1.CertificateRequest, error) {
	log := log.FromContext(ctx)

	// Define a new cert req
	certificateReq, err := r.certReqForVirtualMachine(vm, key)
	if err != nil {
		log.Error(err, "Failed to define new Certificate resource for VirtualMachine")
		return nil, err
	}

	log.Info("Creating a new CertificateRequest", "CertificateRequest.Namespace", certificateReq.Namespace, "CertificateRequest.Name", certificateReq.Name)
	if err = r.Create(ctx, certificateReq); err != nil {
		log.Error(err, "Failed to create new Certificate", "CertificateRequest.Namespace", certificateReq.Namespace, "CertificateRequest.Name", certificateReq.Name)
		return nil, err
	}
	log.Info("Runner CertificateRequest was created", "CertificateRequest.Namespace", certificateReq.Namespace, "CertificateRequest.Name", certificateReq.Name)

	return certificateReq, nil
}

func (r *VMReconciler) createTlsSecret(ctx context.Context, vm *vmv1.VirtualMachine, key crypto.Signer, certificateReq *certv1.CertificateRequest) error {
	log := log.FromContext(ctx)

	certSecret, err := r.certSecretForVirtualMachine(vm, key, certificateReq.Status.Certificate)
	if err != nil {
		log.Error(err, "Failed to define new TLS secret resource for VirtualMachine")
		return err
	}

	if err = r.Create(ctx, certSecret); err != nil {
		log.Error(err, "Failed to create new TLS secret", "Secret.Namespace", certSecret.Namespace, "Secret.Name", certSecret.Name)
		return err
	}
	log.Info("Virtual Machine TLS secret was created", "Secret.Namespace", certSecret.Namespace, "Secret.Name", certSecret.Name)

	return nil
}

func (r *VMReconciler) updateTlsSecret(ctx context.Context, key crypto.Signer, certificateReq *certv1.CertificateRequest, certSecret *corev1.Secret) error {
	log := log.FromContext(ctx)

	encodedKey, err := pki.EncodePrivateKey(key, certv1.PKCS1)
	if err != nil {
		return err
	}
	certSecret.Data[corev1.TLSPrivateKeyKey] = encodedKey
	certSecret.Data[corev1.TLSCertKey] = certificateReq.Status.Certificate

	if err = r.Update(ctx, certSecret); err != nil {
		log.Error(err, "Failed to update new TLS secret", "Secret.Namespace", certSecret.Namespace, "Secret.Name", certSecret.Name)
		return err
	}
	log.Info("Virtual Machine TLS secret was updated", "Secret.Namespace", certSecret.Namespace, "Secret.Name", certSecret.Name)

	return nil
}

func (r *VMReconciler) deleteTmpSecret(ctx context.Context, tmpKeySecret *corev1.Secret) error {
	log := log.FromContext(ctx)

	err := r.Delete(ctx, tmpKeySecret)
	if err != nil {
		log.Info("Virtual Machine temporary TLS private key secret could not be deleted", "Secret.Namespace", tmpKeySecret.Namespace, "Secret.Name", tmpKeySecret.Name)
		return err
	}
	log.Info("VirtualMachine temporary TLS private key secret was deleted", "Secret.Namespace", tmpKeySecret.Namespace, "Secret.Name", tmpKeySecret.Name)
	return nil
}

func (r *VMReconciler) deleteCertRequest(ctx context.Context, certificateReq *certv1.CertificateRequest) error {
	log := log.FromContext(ctx)

	err := r.Delete(ctx, certificateReq)
	if err != nil {
		log.Info("Virtual Machine CertificateRequest could not be deleted", "CertificateRequest.Namespace", certificateReq.Namespace, "CertificateRequest.Name", certificateReq.Name)
		return err
	}
	log.Info("VirtualMachine CertificateRequest was deleted", "CertificateRequest.Namespace", certificateReq.Namespace, "CertificateRequest.Name", certificateReq.Name)
	return nil
}

func certSpecCSR(vm *vmv1.VirtualMachine) (*x509.CertificateRequest, error) {
	certSpec := certv1.CertificateSpec{
		CommonName: vm.Spec.TLS.ServerName,
		DNSNames:   []string{vm.Spec.TLS.ServerName},
		PrivateKey: &certv1.CertificatePrivateKey{
			Algorithm:      certv1.ECDSAKeyAlgorithm,
			Encoding:       certv1.PKCS1,
			RotationPolicy: certv1.RotationPolicyAlways,
			Size:           256,
		},
		Usages:      certv1.DefaultKeyUsages(),
		IsCA:        false,
		Duration:    &metav1.Duration{Duration: vm.Spec.TLS.ExpireAfter.Duration},
		RenewBefore: &metav1.Duration{Duration: vm.Spec.TLS.RenewBefore.Duration},
	}

	cert := &certv1.Certificate{
		Spec: certSpec,
	}

	return pki.GenerateCSR(cert)
}

func tmpKeySecretSpec(
	vm *vmv1.VirtualMachine,
	key crypto.PrivateKey,
) (*corev1.Secret, error) {
	encodedKey, err := pki.EncodePrivateKey(key, certv1.PKCS1)
	if err != nil {
		return nil, err
	}

	name := fmt.Sprintf("%s-tmp", vm.Status.TLSSecretName)

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: vm.Namespace,
		},
		Data: map[string][]byte{
			corev1.TLSPrivateKeyKey: encodedKey,
		},
	}, nil
}

func certSecretSpec(
	vm *vmv1.VirtualMachine,
	key crypto.PrivateKey,
	cert []byte,
) (*corev1.Secret, error) {
	encodedKey, err := pki.EncodePrivateKey(key, certv1.PKCS1)
	if err != nil {
		return nil, err
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vm.Status.TLSSecretName,
			Namespace: vm.Namespace,
		},
		Data: map[string][]byte{
			corev1.TLSPrivateKeyKey: encodedKey,
			corev1.TLSCertKey:       cert,
		},
		Type: corev1.SecretTypeTLS,
	}, nil
}

func certReqSpec(
	vm *vmv1.VirtualMachine,
	key crypto.Signer,
) (*certv1.CertificateRequest, error) {
	issuer := cmmeta.ObjectReference{
		Name:  vm.Spec.TLS.CertificateIssuer,
		Kind:  "ClusterIssuer",
		Group: certmanager.GroupName,
	}

	cr, err := certSpecCSR(vm)
	if err != nil {
		return nil, err
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, cr, key)
	if err != nil {
		return nil, err
	}

	csrPEM := bytes.NewBuffer([]byte{})
	err = pem.Encode(csrPEM, &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER, Headers: map[string]string{}})
	if err != nil {
		return nil, err
	}

	certSpec := certv1.CertificateRequestSpec{
		Duration:  &metav1.Duration{Duration: vm.Spec.TLS.ExpireAfter.Duration},
		IssuerRef: issuer,
		Request:   csrPEM.Bytes(),
		IsCA:      false,
		Usages:    certv1.DefaultKeyUsages(),
	}

	return &certv1.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vm.Name,
			Namespace: vm.Namespace,
		},
		Spec: certSpec,
	}, nil
}
