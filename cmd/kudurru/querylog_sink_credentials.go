package main

import (
	"context"
	"crypto/rand"
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/httpbatch"
)

// querylogSinkSecretLabel marks a Secret as usable by the sinks of
// querylog.sinks. The server's ServiceAccount can read every Secret of its
// namespace, among them its own keys and certificates, and querylog.sinks is
// a setting the API can change. Without the label on the Secret, whoever may
// change the settings could have any of those Secrets presented to a
// destination of their choice. The label is set by whoever may write
// Secrets in the namespace, which the settings API cannot do.
const querylogSinkSecretLabel = "kudurru.io/query-log-sink" //nolint:gosec // G101: a label name, not a credential

// errSecretNotDelegated is a Secret without querylogSinkSecretLabel.
var errSecretNotDelegated = errors.New(`credential reference is not allowed: the Secret needs the label ` + querylogSinkSecretLabel + `: "true"`)

// querylogSecrets reads Secrets for the sinks of querylog.sinks: only in
// Namespace, the one the server runs in, and with the server's own
// ServiceAccount. The settings name a Secret and a key, never a namespace.
type querylogSecrets struct {
	Namespace string
	// Read returns one key of one Secret of Namespace, or
	// errSecretNotDelegated for a Secret the sinks may not use.
	Read func(ctx context.Context, name, key string) ([]byte, error)
}

// querylogKubeSecrets is the Secret reader of the Kubernetes driver; nil
// without a cluster.
func querylogKubeSecrets(restConfig *rest.Config, namespace string) (*querylogSecrets, error) {
	if restConfig == nil {
		return nil, nil
	}
	core, err := corev1client.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return querylogSecretsFrom(core.Secrets(namespace), namespace), nil
}

// querylogSecretsFrom reads labeled Secrets through one namespace's client.
// Every read asks the API server again, so a changed Secret is seen by the
// next read.
func querylogSecretsFrom(secrets corev1client.SecretInterface, namespace string) *querylogSecrets {
	return &querylogSecrets{Namespace: namespace, Read: func(ctx context.Context, name, key string) ([]byte, error) {
		secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if secret.Labels[querylogSinkSecretLabel] != "true" {
			return nil, errSecretNotDelegated
		}
		value, ok := secret.Data[key]
		if !ok {
			return nil, errors.New("the Secret has no such key")
		}
		return value, nil
	}}
}

// querylogSinkCredentials is how a sink of querylog.sinks reads the
// credential its settings reference (D371). querylog.sinks is a cluster
// setting that the API can change, so a reference is honored only as far
// as this node's operator delegated, exactly as for stored provider
// objects: a file or an environment variable must be listed in the node's
// own credentials.allowedFiles or credentials.allowedEnvironment. A Secret
// is read only in the server's namespace and only when it carries
// querylogSinkSecretLabel. Whoever may change the settings can therefore
// not make a node send a file or a Secret of their choice to a destination
// of their choice. A refused reference keeps that sink from opening;
// nothing else is affected.
//
// Every call reads the source again, which is what lets a sink pick up a
// rotated credential after the destination refused the old one.
func querylogSinkCredentials(cfg *config.Config, secrets *querylogSecrets) (httpbatch.Resolver, error) {
	options := credentials.Options{Files: cfg.Credentials.AllowedFiles, Environment: cfg.Credentials.AllowedEnvironment}
	if secrets != nil {
		if secrets.Read == nil {
			return nil, errors.New("querylog: Secret reader is incomplete")
		}
		options.Namespace = secrets.Namespace
		options.Secret = func(ctx context.Context, _, name, key string) ([]byte, error) { return secrets.Read(ctx, name, key) }
	}
	// The resolver fingerprints what it reads for callers that publish a
	// change marker. The sinks compare the values themselves and publish
	// nothing, so a key that lives as long as the process is enough.
	options.FingerprintKey = make([]byte, 32)
	if _, err := rand.Read(options.FingerprintKey); err != nil {
		return nil, err
	}
	resolver, err := credentials.New(options)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, ref config.CredentialRef) ([]byte, error) {
		reference := publicv1.CredentialReference{File: ref.File, Env: ref.Env}
		if ref.SecretRef != nil {
			reference.SecretRef = &publicv1.CredentialSecretRef{Name: ref.SecretRef.Name, Key: ref.SecretRef.Key}
		}
		value, err := resolver.Resolve(ctx, reference)
		switch {
		case errors.Is(err, errSecretNotDelegated):
			// Say what is missing; the resolver reports every failed
			// Secret read as "unavailable".
			return nil, errSecretNotDelegated
		case err != nil:
			return nil, err
		}
		return value.Bytes, nil
	}, nil
}
