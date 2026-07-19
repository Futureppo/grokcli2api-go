package inference

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func TestCLIContractFixtures(t *testing.T) {
	read := func(name string, target any) {
		t.Helper()
		data, err := os.ReadFile("testdata/cli-0.2.102/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	var manifest struct {
		CLIVersion string `json:"cli_version"`
		SourceRev  string `json:"source_rev"`
		Generated  bool   `json:"generated"`
		Sources    []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"sources"`
		Fixtures []string `json:"fixtures"`
	}
	read("manifest.json", &manifest)
	if manifest.CLIVersion != "0.2.102" || manifest.SourceRev != "124d85bc5dc6e7805560215fcc6d5413944920e1" || !manifest.Generated {
		t.Fatalf("manifest=%#v", manifest)
	}
	wantSources := map[string]string{
		"crates/codegen/xai-grok-shell/src/remote/client.rs":     "9c5b3366a71865347970dd8ecf97b49e1728644b6574939e1166ad596a0595b0",
		"crates/codegen/xai-grok-sampling-types/src/messages.rs": "8835ba26ca23f1d36f19c0a553b019feea1f344c7fd3345c46681763e4649c73",
		"crates/codegen/xai-grok-sampling-types/src/types.rs":    "8ec61f64f6f818b2a4ad4fb37ee9d95497e4faca956c5d8e56e444447d6799a1",
		"crates/codegen/xai-grok-sampler/src/client.rs":          "0cfebe106cbb75b4bd33d53ab1059198c0d8ed276fa28c0e6fb114edac18384a",
	}
	if len(manifest.Sources) != len(wantSources) {
		t.Fatalf("manifest sources=%d, want %d", len(manifest.Sources), len(wantSources))
	}
	for _, source := range manifest.Sources {
		if wantSources[source.Path] != source.SHA256 {
			t.Fatalf("unexpected source contract: %#v", source)
		}
	}
	if !reflect.DeepEqual(manifest.Fixtures, []string{"model-descriptors.json", "reasoning-efforts.json"}) {
		t.Fatalf("manifest fixtures=%#v", manifest.Fixtures)
	}
	for _, fixture := range manifest.Fixtures {
		payload, err := os.ReadFile("testdata/cli-0.2.102/" + fixture)
		if err != nil || !json.Valid(payload) {
			t.Fatalf("fixture %q missing or invalid: %v", fixture, err)
		}
	}
	var descriptors []any
	read("model-descriptors.json", &descriptors)
	wantLimits := []uint32{8192, 32768, 16384}
	for index, raw := range descriptors {
		descriptor, ok := modelcatalog.ParseDescriptor(raw)
		if !ok {
			t.Fatalf("descriptor[%d] did not parse: %#v", index, raw)
		}
		if index >= len(wantLimits) || descriptor.MaxCompletionTokens != wantLimits[index] {
			t.Fatalf("descriptor[%d] max completion tokens = %d, want %d", index, descriptor.MaxCompletionTokens, wantLimits[index])
		}
	}
	var efforts []struct {
		Input      string   `json:"input"`
		Supported  []string `json:"supported"`
		Capable    bool     `json:"capable"`
		Normalized string   `json:"normalized"`
	}
	read("reasoning-efforts.json", &efforts)
	for _, fixture := range efforts {
		descriptor := modelcatalog.ModelDescriptor{SupportsReasoningEffort: fixture.Capable, ReasoningEfforts: fixture.Supported}
		if got := modelcatalog.NormalizeReasoningEffort(fixture.Input, descriptor); got != fixture.Normalized {
			t.Fatalf("fixture input %q normalized to %q, want %q", fixture.Input, got, fixture.Normalized)
		}
	}
}

func TestReasoningEffortRendersPerDescriptorAndBackend(t *testing.T) {
	tests := []struct {
		name      string
		effort    string
		supported bool
		list      []string
		want      string
	}{
		{name: "none is always low", effort: "none", supported: true, list: []string{"none", "high"}, want: "low"},
		{name: "minimal", effort: "minimal", supported: true, list: []string{"minimal"}, want: "minimal"},
		{name: "medium", effort: "medium", supported: true, list: []string{"medium"}, want: "medium"},
		{name: "high", effort: "high", supported: true, list: []string{"high"}, want: "high"},
		{name: "xhigh", effort: "xhigh", supported: true, list: []string{"xhigh"}, want: "xhigh"},
		{name: "capability flag without list", effort: "high", supported: true, list: nil, want: "high"},
		{name: "unknown", effort: "vendor-adaptive", supported: true, list: []string{"high"}, want: "low"},
		{name: "known unsupported", effort: "high", supported: true, list: []string{"medium"}, want: "low"},
		{name: "disabled", effort: "high", supported: false, list: nil, want: "low"},
		{name: "case and whitespace", effort: "  XHIGH  ", supported: true, list: []string{"xhigh"}, want: "xhigh"},
	}
	backends := []modelcatalog.Backend{
		modelcatalog.BackendChatCompletions,
		modelcatalog.BackendResponses,
		modelcatalog.BackendMessages,
	}
	for _, test := range tests {
		for _, backend := range backends {
			t.Run(test.name+"/"+string(backend), func(t *testing.T) {
				plan := mustPlan(t, ProtocolResponses, map[string]any{
					"model": "public", "input": "hello", "reasoning": map[string]any{"effort": test.effort},
				})
				attempt, err := plan.Render(modelcatalog.ModelDescriptor{
					ID: "public", WireModel: "wire", Backend: backend,
					SupportsReasoningEffort: test.supported, ReasoningEfforts: test.list,
				})
				if err != nil {
					t.Fatal(err)
				}
				if attempt.ReasoningEffort != test.want {
					t.Fatalf("attempt effort = %q, want %q", attempt.ReasoningEffort, test.want)
				}
				wireWant := test.want
				if backend == modelcatalog.BackendMessages && test.want == "xhigh" {
					wireWant = "max"
				}
				if got := wireEffort(attempt); got != wireWant {
					t.Fatalf("wire effort = %q, want %q; body=%#v", got, wireWant, attempt.Body)
				}
			})
		}
	}
}

func TestMessagesXhighMapsToMaxOnlyWhenSupported(t *testing.T) {
	plan := mustPlan(t, ProtocolMessages, map[string]any{
		"model": "public", "max_tokens": float64(8),
		"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
		"output_config": map[string]any{"effort": "xhigh"},
	})
	supported, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendMessages, SupportsReasoningEffort: true, ReasoningEfforts: []string{"xhigh"}})
	if err != nil || wireEffort(supported) != "max" {
		t.Fatalf("supported attempt=%#v err=%v", supported, err)
	}
	unsupported, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendMessages, SupportsReasoningEffort: true, ReasoningEfforts: []string{"high"}})
	if err != nil || wireEffort(unsupported) != "low" {
		t.Fatalf("unsupported attempt=%#v err=%v", unsupported, err)
	}
}

func TestRetryRerendersWireModelBackendAndReasoning(t *testing.T) {
	plan := mustPlan(t, ProtocolChatCompletions, map[string]any{
		"model": "public", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "high",
	})
	first, err := plan.Render(modelcatalog.ModelDescriptor{
		WireModel: "wire-a", Backend: modelcatalog.BackendResponses,
		SupportsReasoningEffort: true, ReasoningEfforts: []string{"high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := plan.Render(modelcatalog.ModelDescriptor{
		WireModel: "wire-b", Backend: modelcatalog.BackendMessages,
		SupportsReasoningEffort: true, ReasoningEfforts: []string{"medium"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != "responses" || first.Body["model"] != "wire-a" || wireEffort(first) != "high" {
		t.Fatalf("first=%#v", first)
	}
	if second.Path != "messages" || second.Body["model"] != "wire-b" || wireEffort(second) != "low" {
		t.Fatalf("second=%#v", second)
	}
	first.Body["model"] = "mutated"
	again, _ := plan.Render(modelcatalog.ModelDescriptor{WireModel: "wire-a", Backend: modelcatalog.BackendResponses, SupportsReasoningEffort: true, ReasoningEfforts: []string{"high"}})
	if again.Body["model"] != "wire-a" {
		t.Fatal("rendered attempts share mutable state")
	}
}

func TestThreeByThreeRequestRouting(t *testing.T) {
	sources := map[Protocol]map[string]any{
		ProtocolChatCompletions: {
			"model": "public", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"reasoning_effort": "future-effort",
		},
		ProtocolResponses: {
			"model": "public", "input": "hello",
			"reasoning": map[string]any{"effort": "future-effort"},
		},
		ProtocolMessages: {
			"model": "public", "max_tokens": float64(16),
			"messages":      []any{map[string]any{"role": "user", "content": "hello"}},
			"output_config": map[string]any{"effort": "future-effort"},
		},
	}
	backends := map[modelcatalog.Backend]string{
		modelcatalog.BackendChatCompletions: "chat/completions",
		modelcatalog.BackendResponses:       "responses",
		modelcatalog.BackendMessages:        "messages",
	}
	for source, body := range sources {
		for backend, path := range backends {
			t.Run(string(source)+"/"+string(backend), func(t *testing.T) {
				plan := mustPlan(t, source, body)
				attempt, err := plan.Render(modelcatalog.ModelDescriptor{WireModel: "wire", Backend: backend})
				if err != nil {
					t.Fatal(err)
				}
				if attempt.Path != path || attempt.Body["model"] != "wire" {
					t.Fatalf("attempt=%#v", attempt)
				}
				if attempt.ReasoningEffort != "low" || wireEffort(attempt) != "low" {
					t.Fatalf("reasoning was not downgraded per attempt: %#v", attempt)
				}
				switch backend {
				case modelcatalog.BackendChatCompletions:
					if messages, _ := attempt.Body["messages"].([]any); len(messages) == 0 {
						t.Fatalf("chat body=%#v", attempt.Body)
					}
				case modelcatalog.BackendResponses:
					if _, ok := attempt.Body["input"]; !ok {
						t.Fatalf("responses body=%#v", attempt.Body)
					}
					if source != ProtocolResponses && attempt.Body["store"] != false {
						t.Fatalf("cross-protocol store=%#v", attempt.Body["store"])
					}
				case modelcatalog.BackendMessages:
					if messages, _ := attempt.Body["messages"].([]any); len(messages) == 0 || attempt.Body["max_tokens"] == nil {
						t.Fatalf("messages body=%#v", attempt.Body)
					}
				}
			})
		}
	}
}

func TestResponsesStoreDefaultsByCaller(t *testing.T) {
	for _, test := range []struct {
		name   string
		native bool
		want   bool
	}{{"ordinary", false, true}, {"native CLI", true, false}} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := NewRequestPlan(ProtocolResponses, map[string]any{"model": "m", "input": "x"}, PlanOptions{NativeCLI: test.native})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
			if err != nil || attempt.Body["store"] != test.want {
				t.Fatalf("body=%#v err=%v", attempt.Body, err)
			}
		})
	}
}

func TestNativeResponsesPreservesStringConversationID(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m", "input": "x", "conversation": " conv_opaque ",
	})
	native, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if err != nil {
		t.Fatal(err)
	}
	if native.Body["conversation"] != " conv_opaque " {
		t.Fatalf("native conversation = %#v", native.Body["conversation"])
	}
	chat, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := chat.Body["conversation"]; exists {
		t.Fatalf("conversation leaked across protocol: %#v", chat.Body)
	}
}

func TestPlanTenantNamespaceDefaultsToPublic(t *testing.T) {
	body := map[string]any{"model": "m", "input": "x"}
	public, err := NewRequestPlan(ProtocolResponses, body, PlanOptions{})
	if err != nil || public.Tenant() != "public" {
		t.Fatalf("public tenant=%q err=%v", public.Tenant(), err)
	}
	isolated, err := NewRequestPlan(ProtocolResponses, body, PlanOptions{Tenant: " tenant-hmac "})
	if err != nil || isolated.Tenant() != "tenant-hmac" {
		t.Fatalf("isolated tenant=%q err=%v", isolated.Tenant(), err)
	}
}

func TestSilentCleaningDropsStateAndOpaqueContent(t *testing.T) {
	body := map[string]any{
		"model": "m", "previous_response_id": "resp_1", "store": true,
		"conversation": map[string]any{"id": "conv_1"}, "background": true,
		"future_field": map[string]any{"secret": "value"},
		"input": []any{
			map[string]any{"type": "reasoning", "encrypted_content": "opaque"},
			map[string]any{"type": "item_reference", "id": "item_1"},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_file", "file_id": "file_1"},
				map[string]any{"type": "input_text", "text": "safe"},
			}},
		},
	}
	plan := mustPlan(t, ProtocolResponses, body)
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions})
	if err != nil {
		t.Fatal(err)
	}
	if !attempt.DroppedState || attempt.PreservesState {
		t.Fatalf("state flags=%#v", attempt)
	}
	for _, key := range []string{"previous_response_id", "store", "conversation", "background", "future_field"} {
		if _, exists := attempt.Body[key]; exists {
			t.Fatalf("%s leaked: %#v", key, attempt.Body)
		}
	}
	encoded := outputString(attempt.Body, false)
	for _, forbidden := range []string{"opaque", "item_1", "file_1", "secret"} {
		if contains(encoded, forbidden) {
			t.Fatalf("%q leaked: %s", forbidden, encoded)
		}
	}
	if !contains(encoded, "safe") {
		t.Fatalf("safe text was removed: %s", encoded)
	}
}

func TestEncryptedResponsesReasoningIsStatefulOnlyOnNativeBackend(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m", "input": []any{
			map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "ciphertext"},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	})
	native, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if err != nil || !native.PreservesState || native.DroppedState {
		t.Fatalf("native=%#v err=%v", native, err)
	}
	chat, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions})
	if err != nil || chat.PreservesState || !chat.DroppedState || contains(outputString(chat.Body, false), "ciphertext") {
		t.Fatalf("chat=%#v err=%v", chat, err)
	}
}

func TestCleaningErrorsOnlyWhenNoMinimumInputRemains(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m", "input": []any{
			map[string]any{"type": "reasoning", "encrypted_content": "opaque"},
			map[string]any{"type": "input_file", "file_id": "file_1"},
		},
	})
	_, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions})
	if !errors.Is(err, ErrNoRepresentableInput) {
		t.Fatalf("error=%v", err)
	}
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Param != "input" {
		t.Fatalf("typed error=%#v", err)
	}
}

func TestChatModernUserSearchAndStreamUsage(t *testing.T) {
	plan := mustPlan(t, ProtocolChatCompletions, map[string]any{
		"model": "m", "stream": true, "user_id": "u1",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.png", "detail": "high"}},
			map[string]any{"type": "text", "text": "look"},
		}}},
		"search_parameters": map[string]any{
			"mode": "auto", "included_x_handles": []any{"grok"}, "web": true,
			"unknown_legacy_flag": true,
		},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions, SupportsBackendSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Body["user"] != "u1" || attempt.Body["user_id"] != nil {
		t.Fatalf("user fields=%#v", attempt.Body)
	}
	streamOptions, _ := attempt.Body["stream_options"].(map[string]any)
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream_options=%#v", streamOptions)
	}
	search, _ := attempt.Body["search_parameters"].(map[string]any)
	if search["mode"] != "auto" || len(search["sources"].([]any)) != 2 || search["unknown_legacy_flag"] != nil {
		t.Fatalf("search=%#v", search)
	}
	encoded := outputString(attempt.Body, false)
	if contains(encoded, "detail") {
		t.Fatalf("image detail leaked: %s", encoded)
	}
}

func TestChatSearchIntegersFitRustI32(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "fractional", value: float64(1.5)},
		{name: "too large", value: float64(2147483648)},
	} {
		t.Run("max results/"+test.name, func(t *testing.T) {
			plan := mustPlan(t, ProtocolChatCompletions, map[string]any{
				"model": "m", "messages": []any{map[string]any{"role": "user", "content": "find"}},
				"search_parameters": map[string]any{"max_search_results": test.value},
			})
			_, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions, SupportsBackendSearch: true})
			var requestErr *RequestError
			if !errors.As(err, &requestErr) || requestErr.Param != "search_parameters.max_search_results" {
				t.Fatalf("error = %#v", err)
			}
		})
	}

	plan := mustPlan(t, ProtocolChatCompletions, map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "find"}},
		"search_parameters": map[string]any{"sources": []any{map[string]any{
			"type": "x", "post_favorite_count": float64(1.5), "post_view_count": float64(2147483648),
		}}},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions, SupportsBackendSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded := outputString(attempt.Body["search_parameters"], false)
	if contains(encoded, "post_favorite_count") || contains(encoded, "post_view_count") {
		t.Fatalf("out-of-range source count reached wire: %s", encoded)
	}
}

func TestChatAssistantModelIDIsPreservedOnlyAsAString(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		body     map[string]any
		want     string
	}{
		{
			name: "chat", protocol: ProtocolChatCompletions, want: "grok-origin",
			body: map[string]any{"model": "m", "messages": []any{
				map[string]any{"role": "assistant", "model_id": " grok-origin ", "content": "prior"},
				map[string]any{"role": "user", "content": "continue"},
			}},
		},
		{
			name: "responses", protocol: ProtocolResponses, want: "grok-origin",
			body: map[string]any{"model": "m", "input": []any{
				map[string]any{"type": "message", "role": "assistant", "model_id": "grok-origin", "content": "prior"},
				map[string]any{"type": "message", "role": "user", "content": "continue"},
			}},
		},
		{
			name: "messages invalid value", protocol: ProtocolMessages,
			body: map[string]any{"model": "m", "max_tokens": float64(16), "messages": []any{
				map[string]any{"role": "assistant", "model_id": float64(7), "content": "prior"},
				map[string]any{"role": "user", "content": "continue"},
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt, err := mustPlan(t, test.protocol, test.body).Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions})
			if err != nil {
				t.Fatal(err)
			}
			messages := attempt.Body["messages"].([]any)
			assistant := messages[0].(map[string]any)
			if got, _ := assistant["model_id"].(string); got != test.want {
				t.Fatalf("model_id=%q want=%q body=%#v", got, test.want, attempt.Body)
			}
		})
	}
}

func TestMessagesThinkingMapsToResponsesIndependentlyOfEffort(t *testing.T) {
	tests := []struct {
		name          string
		thinking      map[string]any
		wantEffort    string
		wantSummary   bool
		wantEncrypted bool
	}{
		{name: "enabled low budget", thinking: map[string]any{"type": "enabled", "budget_tokens": float64(1024)}, wantEffort: "low", wantSummary: true, wantEncrypted: true},
		{name: "adaptive summarized", thinking: map[string]any{"type": "adaptive", "display": "summarized"}, wantSummary: true, wantEncrypted: true},
		{name: "adaptive omitted", thinking: map[string]any{"type": "adaptive", "display": "omitted"}, wantEncrypted: true},
		{name: "disabled", thinking: map[string]any{"type": "disabled"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := mustPlan(t, ProtocolMessages, map[string]any{
				"model": "m", "max_tokens": float64(32), "thinking": test.thinking,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			})
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
			if err != nil {
				t.Fatal(err)
			}
			reasoning, _ := attempt.Body["reasoning"].(map[string]any)
			if got := trimmedString(reasoning["effort"]); got != test.wantEffort {
				t.Fatalf("effort=%q want=%q body=%#v", got, test.wantEffort, attempt.Body)
			}
			if (trimmedString(reasoning["summary"]) != "") != test.wantSummary {
				t.Fatalf("summary body=%#v", attempt.Body)
			}
			encoded := outputString(attempt.Body["include"], false)
			if contains(encoded, "reasoning.encrypted_content") != test.wantEncrypted {
				t.Fatalf("include=%#v", attempt.Body["include"])
			}
		})
	}
}

func TestMessagesThinkingSignaturePreservesResponsesState(t *testing.T) {
	plan := mustPlan(t, ProtocolMessages, map[string]any{
		"model": "m", "max_tokens": float64(32),
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "summary", "signature": "opaque-signature"}}},
			map[string]any{"role": "user", "content": "continue"},
		},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if err != nil {
		t.Fatal(err)
	}
	if !attempt.PreservesState || attempt.DroppedState {
		t.Fatalf("state flags=%#v", attempt)
	}
	if encoded := outputString(attempt.Body["input"], false); !contains(encoded, "opaque-signature") || !contains(encoded, "summary") {
		t.Fatalf("input=%s", encoded)
	}
}

func TestNativeMessagesPreservesSupportedThinkingCacheAndFormat(t *testing.T) {
	plan := mustPlan(t, ProtocolMessages, map[string]any{
		"model": "m", "max_tokens": float64(64), "top_k": float64(10),
		"system":        []any{map[string]any{"type": "text", "text": "sys", "cache_control": map[string]any{"type": "ephemeral"}}},
		"thinking":      map[string]any{"type": "adaptive", "display": "summarized"},
		"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}},
		"messages":      []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi", "cache_control": map[string]any{"type": "ephemeral"}}}}},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendMessages})
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Body["top_k"] != float64(10) || !reflect.DeepEqual(attempt.Body["thinking"], map[string]any{"type": "adaptive", "display": "summarized"}) {
		t.Fatalf("body=%#v", attempt.Body)
	}
	encoded := outputString(attempt.Body, false)
	for _, required := range []string{"cache_control", "json_schema", "summarized"} {
		if !contains(encoded, required) {
			t.Fatalf("%q missing from %s", required, encoded)
		}
	}
}

func TestCrossProtocolMessagesFormatIsNormalizedAndCleaned(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
		},
	}
	want := map[string]any{
		"type":   "json_schema",
		"schema": schema,
	}
	tests := []struct {
		name     string
		protocol Protocol
		body     map[string]any
	}{
		{
			name:     "chat nested json schema",
			protocol: ProtocolChatCompletions,
			body: map[string]any{
				"model":    "m",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"response_format": map[string]any{
					"type": "json_schema",
					"json_schema": map[string]any{
						"name": "chat_result", "description": "must not leak", "strict": true,
						"schema": schema,
					},
				},
			},
		},
		{
			name:     "responses metadata",
			protocol: ProtocolResponses,
			body: map[string]any{
				"model": "m", "input": "hi",
				"text": map[string]any{"format": map[string]any{
					"type": "json_schema", "name": "responses_result",
					"description": "must not leak", "strict": true, "schema": schema,
				}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := mustPlan(t, test.protocol, test.body)
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendMessages})
			if err != nil {
				t.Fatal(err)
			}
			output, ok := attempt.Body["output_config"].(map[string]any)
			if !ok {
				t.Fatalf("output_config=%#v", attempt.Body["output_config"])
			}
			if got := output["format"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("format=%#v want=%#v", got, want)
			}
		})
	}
}

func TestCrossProtocolMessagesSilentlyDropsUnsupportedFormats(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		body     map[string]any
	}{
		{
			name:     "chat text",
			protocol: ProtocolChatCompletions,
			body: map[string]any{
				"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"response_format": map[string]any{"type": "text"},
			},
		},
		{
			name:     "responses json object",
			protocol: ProtocolResponses,
			body: map[string]any{
				"model": "m", "input": "hi",
				"text": map[string]any{"format": map[string]any{"type": "json_object"}},
			},
		},
		{
			name:     "invalid json schema",
			protocol: ProtocolResponses,
			body: map[string]any{
				"model": "m", "input": "hi",
				"text": map[string]any{"format": map[string]any{
					"type": "json_schema", "name": "missing_schema", "strict": true,
				}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := mustPlan(t, test.protocol, test.body)
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendMessages})
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := attempt.Body["output_config"]; exists {
				t.Fatalf("unsupported format reached wire: %#v", attempt.Body["output_config"])
			}
		})
	}
}

func TestNativeMessagesSilentlyDropsOrphanToolResult(t *testing.T) {
	plan := mustPlan(t, ProtocolMessages, map[string]any{
		"model": "m", "max_tokens": float64(64),
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "missing", "content": "opaque",
			}}},
			map[string]any{"role": "user", "content": "continue"},
		},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendMessages})
	if err != nil {
		t.Fatal(err)
	}
	encoded := outputString(attempt.Body, false)
	if contains(encoded, "opaque") || !contains(encoded, "continue") {
		t.Fatalf("cleaned body=%s", encoded)
	}
}

func TestResponsesToolAliasesAreAttemptLocalAndDescriptorRerendered(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m", "input": "use calendar",
		"tools": []any{
			map[string]any{
				"type": "namespace", "name": "calendar__", "tools": []any{
					map[string]any{"type": "function", "name": "create", "parameters": map[string]any{"type": "object"}},
				},
			},
		},
	})
	responses, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := responses.Body["tools"].([]any)
	if len(tools) != 1 || trimmedString(tools[0].(map[string]any)["name"]) != "calendar__create" {
		t.Fatalf("tools=%#v", tools)
	}
	alias := responses.Adapter.ToolAliases["calendar__create"]
	if alias.Name != "create" || alias.Namespace != "calendar__" || alias.Kind != "function" {
		t.Fatalf("alias=%#v", alias)
	}
	chat, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions})
	if err != nil {
		t.Fatal(err)
	}
	chatAlias := chat.Adapter.ToolAliases["calendar__create"]
	if chatAlias.Name != "create" || chatAlias.Namespace != "calendar__" || chatAlias.Kind != "function" {
		t.Fatalf("chat attempt lost rerendered alias state: %#v", chat.Adapter.ToolAliases)
	}
}

func TestToolChoiceAndParallelIntentRenderAcrossBackends(t *testing.T) {
	tests := []struct {
		name       string
		protocol   Protocol
		body       map[string]any
		chatChoice any
		respChoice any
		msgChoice  any
	}{
		{
			name: "chat named choice", protocol: ProtocolChatCompletions,
			body: map[string]any{
				"model": "m", "messages": []any{map[string]any{"role": "user", "content": "lookup"}},
				"tools":               []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
				"tool_choice":         map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
				"parallel_tool_calls": false,
			},
			chatChoice: map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
			respChoice: map[string]any{"type": "function", "name": "lookup"},
			msgChoice:  map[string]any{"type": "tool", "name": "lookup", "disable_parallel_tool_use": true},
		},
		{
			name: "messages required choice", protocol: ProtocolMessages,
			body: map[string]any{
				"model": "m", "max_tokens": float64(64), "messages": []any{map[string]any{"role": "user", "content": "lookup"}},
				"tools":       []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}},
				"tool_choice": map[string]any{"type": "any", "disable_parallel_tool_use": true},
			},
			chatChoice: "required", respChoice: "required",
			msgChoice: map[string]any{"type": "any", "disable_parallel_tool_use": true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := mustPlan(t, test.protocol, test.body)
			chat, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendChatCompletions})
			if err != nil || !reflect.DeepEqual(chat.Body["tool_choice"], test.chatChoice) {
				t.Fatalf("chat choice=%#v err=%v body=%#v", chat.Body["tool_choice"], err, chat.Body)
			}
			responses, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
			if err != nil || !reflect.DeepEqual(responses.Body["tool_choice"], test.respChoice) || responses.Body["parallel_tool_calls"] != false {
				t.Fatalf("responses choice=%#v err=%v body=%#v", responses.Body["tool_choice"], err, responses.Body)
			}
			messages, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendMessages})
			if err != nil || !reflect.DeepEqual(messages.Body["tool_choice"], test.msgChoice) {
				t.Fatalf("messages choice=%#v err=%v body=%#v", messages.Body["tool_choice"], err, messages.Body)
			}
		})
	}
}

func TestNativeResponsesToolChoiceUsesFlattenedAlias(t *testing.T) {
	for _, test := range []struct {
		name   string
		tool   map[string]any
		choice map[string]any
	}{
		{
			name: "namespace",
			tool: map[string]any{"type": "namespace", "name": "calendar__", "tools": []any{
				map[string]any{"type": "function", "name": "create", "parameters": map[string]any{"type": "object"}},
			}},
			choice: map[string]any{"type": "function", "name": "create", "namespace": "calendar__"},
		},
		{
			name:   "custom",
			tool:   map[string]any{"type": "custom", "name": "shell", "description": "run code"},
			choice: map[string]any{"type": "custom", "name": "shell"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := mustPlan(t, ProtocolResponses, map[string]any{
				"model": "m", "input": "use tool", "tools": []any{test.tool}, "tool_choice": test.choice,
			})
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
			if err != nil {
				t.Fatal(err)
			}
			choice, _ := attempt.Body["tool_choice"].(map[string]any)
			wireName := trimmedString(choice["name"])
			if trimmedString(choice["type"]) != "function" || wireName == "" {
				t.Fatalf("choice=%#v body=%#v", choice, attempt.Body)
			}
			if alias, ok := attempt.Adapter.RestoreTool(wireName); !ok || alias.Name != trimmedString(test.choice["name"]) {
				t.Fatalf("wire choice %q has no matching alias: %#v", wireName, attempt.Adapter.ToolAliases)
			}
		})
	}
}

func TestResponsesExtendedToolsRemainPortableAcrossBackends(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m", "input": "use tools",
		"tools": []any{
			map[string]any{"type": "namespace", "name": "calendar__", "tools": []any{
				map[string]any{"type": "function", "name": "create", "parameters": map[string]any{"type": "object"}},
			}},
			map[string]any{"type": "custom", "name": "shell", "description": "run code"},
		},
	})
	for _, backend := range []modelcatalog.Backend{modelcatalog.BackendChatCompletions, modelcatalog.BackendMessages} {
		t.Run(string(backend), func(t *testing.T) {
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			encoded := outputString(attempt.Body["tools"], false)
			for _, name := range []string{"calendar__create", "shell"} {
				if !contains(encoded, name) {
					t.Fatalf("%s tool missing from %s wire: %s", name, backend, encoded)
				}
			}
			calendar := attempt.Adapter.ToolAliases["calendar__create"]
			shell := attempt.Adapter.ToolAliases["shell"]
			if calendar.Name != "create" || calendar.Namespace != "calendar__" || shell.Name != "shell" || shell.Kind != "custom" {
				t.Fatalf("aliases=%#v", attempt.Adapter.ToolAliases)
			}
			if _, exists := attempt.Body["tool_choice"]; exists {
				t.Fatalf("missing public tool choice became an empty wire object: %#v", attempt.Body["tool_choice"])
			}
			if !contains(encoded, `"input"`) {
				t.Fatalf("custom tool wrapper schema missing: %s", encoded)
			}
		})
	}
}

func TestNativeMessagesDoesNotSynthesizeEmptyToolChoice(t *testing.T) {
	plan := mustPlan(t, ProtocolMessages, map[string]any{
		"model": "m", "max_tokens": float64(64),
		"messages": []any{map[string]any{"role": "user", "content": "lookup"}},
		"tools":    []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendMessages})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := attempt.Body["tool_choice"]; exists {
		t.Fatalf("tool_choice=%#v", attempt.Body["tool_choice"])
	}
}

func TestResponsesCustomToolHistoryUsesPortableObjectWrapper(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "shell", "input": "echo hello"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "hello"},
		},
		"tools": []any{map[string]any{"type": "custom", "name": "shell"}},
	})
	for _, backend := range []modelcatalog.Backend{modelcatalog.BackendChatCompletions, modelcatalog.BackendMessages} {
		t.Run(string(backend), func(t *testing.T) {
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			messages := attempt.Body["messages"].([]any)
			wrapped := false
			if backend == modelcatalog.BackendChatCompletions {
				calls := messages[0].(map[string]any)["tool_calls"].([]any)
				function := calls[0].(map[string]any)["function"].(map[string]any)
				wrapped = function["arguments"] == `{"input":"echo hello"}`
			} else {
				content := messages[0].(map[string]any)["content"].([]any)
				input, _ := content[0].(map[string]any)["input"].(map[string]any)
				wrapped = input["input"] == "echo hello"
			}
			if !wrapped {
				t.Fatalf("custom history was not wrapped on %s: %#v", backend, attempt.Body)
			}
		})
	}
}

func TestToolSearchContinuationLoadsToolsWithoutUnresolvedShimCall(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "tool_search_call", "call_id": "search_1", "execution": "client", "arguments": map[string]any{"goal": "calendar"}},
			map[string]any{"type": "tool_search_output", "call_id": "search_1", "execution": "client", "tools": []any{
				map[string]any{"type": "function", "name": "calendar_create", "defer_loading": true, "parameters": map[string]any{"type": "object"}},
			}},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	})
	for _, backend := range []modelcatalog.Backend{modelcatalog.BackendChatCompletions, modelcatalog.BackendResponses, modelcatalog.BackendMessages} {
		t.Run(string(backend), func(t *testing.T) {
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			encoded := outputString(attempt.Body, false)
			if !contains(encoded, "calendar_create") || !contains(encoded, toolSearchCompletedText) || contains(encoded, "grokcli2api_tool_search") {
				t.Fatalf("tool-search continuation on %s=%s", backend, encoded)
			}
		})
	}
}

func TestNativeResponsesHardensToolArgumentAndOutputShapes(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": map[string]any{"city": "Paris"}},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{
				map[string]any{"type": "text", "text": "sunny"}, map[string]any{"temperature": 26},
			}},
		},
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if err != nil {
		t.Fatal(err)
	}
	input := attempt.Body["input"].([]any)
	call, output := input[0].(map[string]any), input[1].(map[string]any)
	if call["arguments"] != `{"city":"Paris"}` || output["output"] != "sunny\n{\"temperature\":26}" {
		t.Fatalf("input=%#v", input)
	}
}

func TestResponsesHistoryBuildsAliasesWithoutRepeatedToolDefinitions(t *testing.T) {
	for _, test := range []struct {
		name string
		call map[string]any
		kind string
	}{
		{
			name: "namespace",
			call: map[string]any{"type": "function_call", "call_id": "call_1", "name": "create", "namespace": "calendar__", "arguments": `{}`},
			kind: "function",
		},
		{
			name: "custom",
			call: map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "shell", "input": "echo hi"},
			kind: "custom",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := mustPlan(t, ProtocolResponses, map[string]any{
				"model": "m", "input": []any{
					test.call,
					map[string]any{"type": map[string]string{"function": "function_call_output", "custom": "custom_tool_call_output"}[test.kind], "call_id": "call_1", "output": "done"},
				},
			})
			attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
			if err != nil {
				t.Fatal(err)
			}
			input := attempt.Body["input"].([]any)
			wireCall := input[0].(map[string]any)
			wireName := trimmedString(wireCall["name"])
			alias, ok := attempt.Adapter.RestoreTool(wireName)
			if !ok || alias.Kind != test.kind || wireCall["type"] != "function_call" || wireCall["namespace"] != nil {
				t.Fatalf("input=%#v aliases=%#v", input, attempt.Adapter.ToolAliases)
			}
		})
	}
}

func TestNativeResponsesKeepsToolSearchContractAndDefaultsFunctionSchema(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m", "input": "find a tool",
		"tools": []any{
			map[string]any{"type": "function", "name": "parameterless"},
			map[string]any{
				"type": "tool_search", "description": "Find the exact integration",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}},
			},
		},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if err != nil {
		t.Fatal(err)
	}
	tools := attempt.Body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%#v", tools)
	}
	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		byName[trimmedString(tool["name"])] = tool
	}
	parameterless := byName["parameterless"]
	search := byName["grokcli2api_tool_search"]
	searchSchema, _ := search["parameters"].(map[string]any)
	properties, _ := searchSchema["properties"].(map[string]any)
	if parameterless == nil || search == nil || !contains(stringValue(search["description"]), "exact integration") || properties["query"] == nil {
		t.Fatalf("tools=%#v", tools)
	}
}

func TestResponsesRenderingPreservesContentAndToolItemOrder(t *testing.T) {
	plan := mustPlan(t, ProtocolChatCompletions, map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "before", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result"},
		},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if err != nil {
		t.Fatal(err)
	}
	input := attempt.Body["input"].([]any)
	if len(input) != 3 || trimmedString(input[0].(map[string]any)["type"]) != "message" ||
		trimmedString(input[1].(map[string]any)["type"]) != "function_call" ||
		trimmedString(input[2].(map[string]any)["type"]) != "function_call_output" {
		t.Fatalf("input order=%#v", input)
	}
}

func TestNativeResponsesSilentlyDropsOrphanToolOutput(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "lost", "output": "secret"},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	})
	attempt, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if err != nil {
		t.Fatal(err)
	}
	input := attempt.Body["input"].([]any)
	if len(input) != 1 || trimmedString(input[0].(map[string]any)["type"]) != "message" {
		t.Fatalf("cleaned input=%#v", input)
	}
	if contains(outputString(attempt.Body, false), "secret") {
		t.Fatalf("orphan output reached wire: %#v", attempt.Body)
	}
}

func TestNativeResponsesOrphanOnlyBecomesNoInput(t *testing.T) {
	plan := mustPlan(t, ProtocolResponses, map[string]any{
		"model": "m", "input": []any{map[string]any{
			"type": "function_call_output", "call_id": "lost", "output": "secret",
		}},
	})
	_, err := plan.Render(modelcatalog.ModelDescriptor{Backend: modelcatalog.BackendResponses})
	if !errors.Is(err, ErrNoRepresentableInput) {
		t.Fatalf("error=%#v", err)
	}
}

func TestRetainedEffortMustBeString(t *testing.T) {
	_, err := NewRequestPlan(ProtocolResponses, map[string]any{
		"model": "m", "input": "x", "reasoning": map[string]any{"effort": 7},
	}, PlanOptions{})
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Param != "reasoning.effort" {
		t.Fatalf("error=%#v", err)
	}
}

func wireEffort(attempt *RenderedAttempt) string {
	switch attempt.Backend {
	case modelcatalog.BackendChatCompletions:
		return trimmedString(attempt.Body["reasoning_effort"])
	case modelcatalog.BackendResponses:
		reasoning, _ := attempt.Body["reasoning"].(map[string]any)
		return trimmedString(reasoning["effort"])
	case modelcatalog.BackendMessages:
		output, _ := attempt.Body["output_config"].(map[string]any)
		return trimmedString(output["effort"])
	default:
		return ""
	}
}

func mustPlan(t *testing.T, protocol Protocol, body map[string]any) *RequestPlan {
	t.Helper()
	plan, err := NewRequestPlan(protocol, body, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func contains(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
