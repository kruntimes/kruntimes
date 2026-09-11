package gateway

import (
	"context"
	"crypto/x509"
	"net/http"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultAuthorizationTimeout = 2 * time.Second

// KubernetesAuthorizer authenticates a bearer token or a TLS client
// certificate verified by the Gateway, then authorizes get access to the
// target Run with the authenticated Kubernetes user identity.
type KubernetesAuthorizer struct {
	Client  kubernetes.Interface
	Timeout time.Duration
}

func (a KubernetesAuthorizer) Authorize(ctx context.Context, request *http.Request, run *v1alpha1.Run) error {
	if a.Client == nil {
		return status.Error(codes.FailedPrecondition, "Kubernetes authorization client is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	user, err := authenticatedGatewayUser(ctx, a.Client, request)
	if err != nil {
		return err
	}
	return AuthorizeRunUser(ctx, a.Client, user, run, a.timeout())
}

// AuthorizeRunUser checks the same Kubernetes Run permission used by the
// direct Gateway, but accepts an identity which has already been authenticated
// by a trusted Kubernetes API aggregation proxy.
func AuthorizeRunUser(ctx context.Context, kubernetesClient kubernetes.Interface, user authenticationv1.UserInfo, run *v1alpha1.Run, timeout time.Duration) error {
	if kubernetesClient == nil {
		return status.Error(codes.FailedPrecondition, "Kubernetes authorization client is not configured")
	}
	if run == nil {
		return status.Error(codes.NotFound, "Run not found")
	}
	if timeout <= 0 {
		timeout = defaultAuthorizationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	subjectAccessReview, err := kubernetesClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user.Username,
			UID:    user.UID,
			Groups: user.Groups,
			Extra:  authorizationExtra(user.Extra),
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:      "get",
				Group:     v1alpha1.GroupVersion.Group,
				Resource:  "runs",
				Namespace: run.Namespace,
				Name:      run.Name,
			},
		},
	}, metav1CreateOptions)
	if err != nil {
		return status.Errorf(codes.Unavailable, "authorize Run: %v", err)
	}
	if !subjectAccessReview.Status.Allowed {
		return status.Error(codes.PermissionDenied, "not authorized to access Run")
	}
	return nil
}

func authenticatedGatewayUser(ctx context.Context, kubernetesClient kubernetes.Interface, request *http.Request) (authenticationv1.UserInfo, error) {
	if token, ok := bearerToken(request.Header.Get("Authorization")); ok {
		tokenReview, err := kubernetesClient.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
			Spec: authenticationv1.TokenReviewSpec{Token: token},
		}, metav1CreateOptions)
		if err != nil {
			return authenticationv1.UserInfo{}, status.Errorf(codes.Unavailable, "authenticate bearer token: %v", err)
		}
		if !tokenReview.Status.Authenticated {
			return authenticationv1.UserInfo{}, status.Error(codes.Unauthenticated, "bearer token is not authenticated")
		}
		return tokenReview.Status.User, nil
	}
	if certificate := verifiedClientCertificate(request); certificate != nil {
		if certificate.Subject.CommonName == "" {
			return authenticationv1.UserInfo{}, status.Error(codes.Unauthenticated, "verified client certificate has no Kubernetes username")
		}
		return authenticationv1.UserInfo{Username: certificate.Subject.CommonName, Groups: certificate.Subject.Organization}, nil
	}
	return authenticationv1.UserInfo{}, status.Error(codes.Unauthenticated, "bearer token is required unless a verified client certificate is presented")
}

func verifiedClientCertificate(request *http.Request) *x509.Certificate {
	if request == nil || request.TLS == nil || len(request.TLS.PeerCertificates) == 0 || len(request.TLS.VerifiedChains) == 0 {
		return nil
	}
	return request.TLS.PeerCertificates[0]
}

func authorizationExtra(extra map[string]authenticationv1.ExtraValue) map[string]authorizationv1.ExtraValue {
	if len(extra) == 0 {
		return nil
	}
	result := make(map[string]authorizationv1.ExtraValue, len(extra))
	for key, values := range extra {
		result[key] = authorizationv1.ExtraValue(values)
	}
	return result
}

func (a KubernetesAuthorizer) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return defaultAuthorizationTimeout
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

var metav1CreateOptions = metav1.CreateOptions{}
