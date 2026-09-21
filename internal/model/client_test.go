// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/ax/internal/model"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestClient_Default(t *testing.T) {
	client := model.NewDefaultClient(model.WithDisableRemote(true))
	if client.Config().Model != model.DefaultModel {
		t.Fatalf("expected default model %q, got %q", model.DefaultModel, client.Config().Model)
	}
	if client.Config().Provider != model.ProviderGoogle {
		t.Fatalf("expected provider %q, got %q", model.ProviderGoogle, client.Config().Provider)
	}
	if client.Config().Name != model.DefaultModelResourceName {
		t.Fatalf("expected default name %q, got %q", model.DefaultModelResourceName, client.Config().Name)
	}
	if client.Config().SecretKey == nil {
		t.Fatalf("expected default secretKey not nil")
	}
	if client.Config().SecretKey.Name != model.DefaultSecretName || client.Config().SecretKey.Key != model.DefaultSecretKey {
		t.Errorf("expected secretKey %s/%s, got %s/%s", model.DefaultSecretName, model.DefaultSecretKey, client.Config().SecretKey.Name, client.Config().SecretKey.Key)
	}
	if len(client.Config().Parameters) != 0 {
		t.Errorf("expected default parameters to be empty, got %v", client.Config().Parameters)
	}

	resp, err := client.Generate(context.Background(), &model.GenerateRequest{
		Prompt: "Configure Go workspace",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Model != model.DefaultModel {
		t.Errorf("expected model %q, got %q", model.DefaultModel, resp.Model)
	}
	if resp.Content == "" {
		t.Errorf("expected non-empty content")
	}
}

func TestClient_FromConfig(t *testing.T) {
	cfg := model.Config{
		Provider: "google",
		Model:    "gemini-3.8-flash",
		Parameters: map[string]any{
			"temperature":       0.3,
			"maxOutputTokens":   2048,
			"systemInstruction": "System workspace planner",
		},
		DisableRemote: true,
	}

	client := model.NewClient(cfg)
	if client.Config().Model != "gemini-3.8-flash" {
		t.Errorf("expected model gemini-3.8-flash, got %s", client.Config().Model)
	}
	if client.Config().Parameters["temperature"] != 0.3 {
		t.Errorf("expected temperature 0.3, got %v", client.Config().Parameters["temperature"])
	}

	resp, err := client.Generate(context.Background(), &model.GenerateRequest{
		Prompt: "Bootstrap workspace",
	})
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	if resp.Model != "gemini-3.8-flash" {
		t.Errorf("expected gemini-3.8-flash, got %s", resp.Model)
	}
}

func TestClient_FromCRD(t *testing.T) {
	crd := &v1alpha1.Model{
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "gemini-flash",
			Atespace: "default",
		},
		Spec: &v1alpha1.ModelSpec{
			Provider:   "google",
			Model:      "gemini-3.8-flash",
			Parameters: params(t, map[string]any{"temperature": 0.4, "maxOutputTokens": 1024}),
		},
	}

	client := model.NewClientFromCRD(crd, model.WithDisableRemote(true))
	if client.Config().Model != "gemini-3.8-flash" {
		t.Errorf("expected model gemini-3.8-flash, got %s", client.Config().Model)
	}

	resp, err := client.Generate(context.Background(), &model.GenerateRequest{
		Prompt: "Bootstrap workspace",
	})
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	if resp.Model != "gemini-3.8-flash" {
		t.Errorf("expected gemini-3.8-flash, got %s", resp.Model)
	}
}

func TestClient_GeminiHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !r.URL.Query().Has("key") {
			t.Errorf("expected key in query params")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"candidates": []map[string]interface{}{
				{
					"content": map[string]interface{}{
						"parts": []map[string]string{
							{"text": "Setup Go environment with Go 1.27"},
						},
					},
				},
			},
			"usageMetadata": map[string]int{
				"promptTokenCount":     10,
				"candidatesTokenCount": 20,
				"totalTokenCount":      30,
			},
		})
	}))
	defer ts.Close()

	client := model.NewClient(
		model.Config{
			Provider: "google",
			Model:    "gemini-3.8-flash",
			BaseURL:  ts.URL,
			APIKey:   "test-api-key",
		},
		model.WithHTTPClient(ts.Client()),
	)

	resp, err := client.Generate(context.Background(), &model.GenerateRequest{
		Prompt: "Plan Go setup",
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if resp.Content != "Setup Go environment with Go 1.27" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if resp.Usage.TotalTokens != 30 {
		t.Errorf("expected 30 total tokens, got %d", resp.Usage.TotalTokens)
	}
}

func TestClient_DefaultModelSecretResolution(t *testing.T) {
	// 1. Resolve via in-cluster Kubernetes API simulation
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/default/secrets/gemini-api-secret" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{
					"GEMINI_API_KEY": "a3ViZXJuZXRlcy1zZWNyZXQtdmFsdWUtNTU1", // base64 for "kubernetes-secret-value-555"
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	// Parse host and port from test server URL
	hostPort := strings.TrimPrefix(ts.URL, "http://")
	parts := strings.Split(hostPort, ":")
	t.Setenv("KUBERNETES_SERVICE_HOST", parts[0])
	t.Setenv("KUBERNETES_SERVICE_PORT", parts[1])

	// Create temp token file
	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, "token")
	_ = os.WriteFile(tokenFile, []byte("fake-token"), 0644)

	// 2. Resolve via WithSecretResolver (standard in-memory / test pattern)
	clientResolver := model.NewDefaultClient(
		model.WithSecretResolver(func(secretName, key string) (string, error) {
			if secretName == "gemini-api-secret" && key == "GEMINI_API_KEY" {
				return "resolved-via-custom-func-333", nil
			}
			return "", nil
		}),
		model.WithDisableRemote(true),
	)
	if clientResolver.Config().APIKey != "resolved-via-custom-func-333" {
		t.Errorf("expected APIKey from custom resolver, got %q", clientResolver.Config().APIKey)
	}
}

func TestClient_DefaultModelStore(t *testing.T) {
	ctx := context.Background()
	s := memory.NewStore()

	// Store a customized default-model CRD
	customModel := &v1alpha1.Model{
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "default-model",
			Atespace: "default",
		},
		Spec: &v1alpha1.ModelSpec{
			Provider:   "google",
			Model:      "gemini-3.8-flash",
			Parameters: params(t, map[string]any{"temperature": 0.7, "maxOutputTokens": 4096}),
			SecretKey: &v1alpha1.SecretKeyRef{
				Name: "custom-gemini-secret",
				Key:  "CUSTOM_KEY",
			},
		},
	}
	if err := s.SaveModel(ctx, customModel); err != nil {
		t.Fatalf("failed to save model: %v", err)
	}

	secretResolverOpt := model.WithSecretResolver(func(secretName, key string) (string, error) {
		if secretName == "custom-gemini-secret" && key == "CUSTOM_KEY" {
			return "store-secret-resolved-999", nil
		}
		return "", nil
	})

	// Read using NewDefaultClientFromStore
	client, err := model.NewDefaultClientFromStore(ctx, s, "default", secretResolverOpt, model.WithDisableRemote(true))
	if err != nil {
		t.Fatalf("NewDefaultClientFromStore failed: %v", err)
	}

	if client.Config().Name != "default-model" {
		t.Errorf("expected name default-model, got %s", client.Config().Name)
	}
	if client.Config().Parameters["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", client.Config().Parameters["temperature"])
	}
	if client.Config().Parameters["maxOutputTokens"] != float64(4096) {
		t.Errorf("expected maxOutputTokens 4096, got %v", client.Config().Parameters["maxOutputTokens"])
	}
	if client.Config().APIKey != "store-secret-resolved-999" {
		t.Errorf("expected APIKey 'store-secret-resolved-999', got %q", client.Config().APIKey)
	}

	// Read using WithStore option
	clientWithStore := model.NewDefaultClient(
		model.WithStore(ctx, s, "default", "default-model"),
		secretResolverOpt,
		model.WithDisableRemote(true),
	)
	if clientWithStore.Config().Parameters["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", clientWithStore.Config().Parameters["temperature"])
	}
	if clientWithStore.Config().APIKey != "store-secret-resolved-999" {
		t.Errorf("expected APIKey 'store-secret-resolved-999', got %q", clientWithStore.Config().APIKey)
	}
}

// params builds a Struct for a ModelSpec's parameters, failing the test on
// unsupported value types.
func params(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClient_AnthropicHTTP(t *testing.T) {
	var got map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected path /v1/messages, got %s", r.URL.Path)
		}
		if h := r.Header.Get("x-api-key"); h != "test-anthropic-key" {
			t.Errorf("expected x-api-key header, got %q", h)
		}
		if h := r.Header.Get("anthropic-version"); h == "" {
			t.Errorf("expected anthropic-version header")
		}
		if r.URL.Query().Has("key") {
			t.Errorf("api key must not be sent as a query parameter")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"type":  "message",
			"model": "claude-opus-5",
			"content": []map[string]string{
				{"type": "thinking", "thinking": "internal reasoning that is not the answer"},
				{"type": "text", "text": "Setup Go environment "},
				{"type": "text", "text": "with Go 1.27"},
			},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 20},
		})
	}))
	defer ts.Close()

	cfg := model.Config{
		Provider: model.ProviderAnthropic,
		Model:    "claude-opus-5",
		Parameters: map[string]any{
			"maxTokens":         16000,
			"top_p":             0.9,
			"systemInstruction": "be brief",
		},
	}
	c := model.NewClient(cfg, model.WithAPIKey("test-anthropic-key"), model.WithBaseURL(ts.URL))

	resp, err := c.Generate(context.Background(), &model.GenerateRequest{Prompt: "plan it", Temperature: 0.5})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if resp.Content != "Setup Go environment with Go 1.27" {
		t.Errorf("expected only text blocks concatenated, got %q", resp.Content)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 20 || resp.Usage.TotalTokens != 30 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}

	if got["model"] != "claude-opus-5" {
		t.Errorf("expected model in body, got %v", got["model"])
	}
	if got["max_tokens"] != float64(16000) {
		t.Errorf("expected maxTokens parameter sent as max_tokens=16000, got %v", got["max_tokens"])
	}
	if _, ok := got["maxTokens"]; ok {
		t.Errorf("maxTokens must be renamed, not sent as-is")
	}
	if got["top_p"] != 0.9 {
		t.Errorf("expected other parameters passed through, got top_p=%v", got["top_p"])
	}
	if got["system"] != "be brief" {
		t.Errorf("expected systemInstruction sent as system, got %v", got["system"])
	}
	if got["temperature"] != 0.5 {
		t.Errorf("expected per-request temperature, got %v", got["temperature"])
	}
	for _, k := range []string{"systemInstruction", "baseURL"} {
		if _, ok := got[k]; ok {
			t.Errorf("client parameter %q must not be sent to the API", k)
		}
	}
	msgs, _ := got["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %v", got["messages"])
	}
	if m, _ := msgs[0].(map[string]interface{}); m["role"] != "user" || m["content"] != "plan it" {
		t.Errorf("unexpected message: %v", msgs[0])
	}
}

// A self-hosted server that speaks the Messages API is reached by setting
// baseURL in the Model parameters, and needs no API key.
func TestClient_AnthropicSelfHostedBaseURLParameter(t *testing.T) {
	var got map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected path /v1/messages, got %s", r.URL.Path)
		}
		if h := r.Header.Get("x-api-key"); h != "" {
			t.Errorf("expected no x-api-key header without a key, got %q", h)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": "pong"}},
			"usage":   map[string]int{"input_tokens": 3, "output_tokens": 1},
		})
	}))
	defer ts.Close()

	params, err := structpb.NewStruct(map[string]any{"baseURL": ts.URL + "/"})
	if err != nil {
		t.Fatalf("building parameters: %v", err)
	}
	crd := &v1alpha1.Model{
		Metadata: &v1alpha1.ObjectMeta{Name: "self-hosted", Atespace: "default"},
		Spec: &v1alpha1.ModelSpec{
			Provider:   model.ProviderAnthropic,
			Model:      "deepseek-v4-flash",
			Parameters: params,
			SecretKey:  &v1alpha1.SecretKeyRef{Name: "none", Key: "none"},
		},
	}
	c := model.NewClient(model.ConfigFromCRD(crd), model.WithSecretResolver(func(string, string) (string, error) { return "", nil }))

	resp, err := c.Generate(context.Background(), &model.GenerateRequest{Prompt: "ping"})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if resp.Content != "pong" {
		t.Errorf("expected a real response from the self-hosted endpoint, got %q", resp.Content)
	}
	if got["max_tokens"] != float64(4096) {
		t.Errorf("expected default max_tokens, got %v", got["max_tokens"])
	}
	if _, ok := got["baseURL"]; ok {
		t.Errorf("baseURL must not be sent to the API")
	}
}

func TestClient_AnthropicAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`, http.StatusBadRequest)
	}))
	defer ts.Close()

	c := model.NewClient(model.Config{Provider: model.ProviderAnthropic, Model: "nope"},
		model.WithAPIKey("k"), model.WithBaseURL(ts.URL))
	_, err := c.Generate(context.Background(), &model.GenerateRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "anthropic api error 400") {
		t.Errorf("expected an anthropic api error, got %v", err)
	}
}

func TestClient_UnsupportedProvider(t *testing.T) {
	c := model.NewClient(model.Config{Provider: "nonesuch", Model: "m"}, model.WithAPIKey("k"))
	_, err := c.Generate(context.Background(), &model.GenerateRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), `unsupported provider "nonesuch"`) {
		t.Errorf("expected unsupported provider error, got %v", err)
	}
}

// The Gemini request must not carry client parameters either.
func TestClient_GeminiDoesNotForwardClientParameters(t *testing.T) {
	var got map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{})
	}))
	defer ts.Close()

	c := model.NewClient(model.Config{
		Provider:   model.ProviderGoogle,
		Model:      "gemini-3.8-flash",
		Parameters: map[string]any{"baseURL": ts.URL, "temperature": 0.2},
	}, model.WithAPIKey("k"))
	if _, err := c.Generate(context.Background(), &model.GenerateRequest{Prompt: "x"}); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	gc, _ := got["generationConfig"].(map[string]interface{})
	if gc["temperature"] != 0.2 {
		t.Errorf("expected temperature passed through, got %v", gc)
	}
	if _, ok := gc["baseURL"]; ok {
		t.Errorf("baseURL must not be sent in generationConfig")
	}
}
