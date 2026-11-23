package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Exchanger выпускает k8s-токен SA с audience федерации и меняет его на IAM-токен YC SA (RFC 8693).
type Exchanger struct {
	client   kubernetes.Interface
	audience string
	tokenURL string
	http     *http.Client
}

func NewExchanger(client kubernetes.Interface, audience string) *Exchanger {
	return &Exchanger{
		client:   client,
		audience: audience,
		tokenURL: "https://auth.yandex.cloud/oauth/token",
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Token возвращает IAM-токен и момент его истечения.
func (e *Exchanger) Token(ctx context.Context, id Identity) (string, time.Time, error) {
	ttl := int64(600) // минимум TokenRequest; токен сразу уходит в обмен
	tr, err := e.client.CoreV1().ServiceAccounts(id.Namespace).CreateToken(ctx, id.ServiceAccount,
		&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{Audiences: []string{e.audience}, ExpirationSeconds: &ttl}},
		metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token request: %w", err)
	}

	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"audience":             {id.YCServiceAccountID},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:id_token"},
		"subject_token":        {tr.Status.Token},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("exchange: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("exchange: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", time.Time{}, fmt.Errorf("exchange: http %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("exchange: http %d: %s: %s", resp.StatusCode, body.Error, body.ErrorDescription)
	}
	return body.AccessToken, time.Now().Add(time.Duration(body.ExpiresIn) * time.Second), nil
}
