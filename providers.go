package blueship

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/provider/anthropic"
	"github.com/rasimio/blueship/internal/provider/anthropicoauth"
	"github.com/rasimio/blueship/internal/provider/gemini"
	"github.com/rasimio/blueship/internal/provider/ollama"
	"github.com/rasimio/blueship/internal/provider/openai"
	"github.com/rasimio/blueship/internal/provider/openaicodex"
	"github.com/rasimio/blueship/internal/transport/telegram"
	"github.com/rasimio/blueship/internal/webaccess/web"
)

// --- Convenience constructors for Config ---

// Anthropic creates a CompletionProvider using the Anthropic Messages API.
func Anthropic(apiKey string) CompletionProvider {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return anthropic.NewProvider(
		apiKey,
		120*time.Second,
		[]time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second},
		logger,
	)
}

// AnthropicWithConfig creates a CompletionProvider with custom timeout and retry settings.
func AnthropicWithConfig(apiKey string, timeout time.Duration, backoffs []time.Duration) CompletionProvider {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return anthropic.NewProvider(apiKey, timeout, backoffs, logger)
}

// OpenAI creates a CompletionProvider using OpenAI Chat Completions.
func OpenAI(apiKey string) CompletionProvider {
	return openai.NewCompletionProvider(apiKey, 120*time.Second)
}

// OpenAIWithConfig creates a CompletionProvider with a custom timeout.
func OpenAIWithConfig(apiKey string, timeout time.Duration) CompletionProvider {
	return openai.NewCompletionProvider(apiKey, timeout)
}

// OpenAICompatible creates a CompletionProvider for any OpenAI-compatible API
// (vLLM, LM Studio, etc.). Pass empty apiKey if auth is not required.
// extraParams are merged into every request JSON (e.g. for chat_template_kwargs).
// For Ollama prefer Ollama() below — its /v1/ endpoint has bugs around the
// Gemma reasoning field.
func OpenAICompatible(baseURL, apiKey string, timeout time.Duration, extraParams map[string]any) CompletionProvider {
	return openai.NewCompatibleProvider(baseURL, apiKey, timeout, extraParams)
}

// Ollama creates a CompletionProvider that speaks Ollama's native /api/chat
// protocol (NDJSON streaming, options-nested generation params, think=false).
// Pass empty baseURL for http://localhost:11434.
//
// keepAlive is how long the server should hold the model resident between
// requests ("30m", seconds as a number, or -1 for indefinitely); nil keeps
// Ollama's five-minute default. See NewCompletionProvider for why an
// interactive caller should almost always set it.
func Ollama(baseURL string, timeout time.Duration, keepAlive any) CompletionProvider {
	return ollama.NewCompletionProvider(baseURL, timeout, keepAlive)
}

// Gemini creates a CompletionProvider using Gemini generateContent.
func Gemini(apiKey string) CompletionProvider {
	return gemini.NewCompletionProvider(apiKey, 120*time.Second)
}

// GeminiWithConfig creates a CompletionProvider with a custom timeout.
func GeminiWithConfig(apiKey string, timeout time.Duration) CompletionProvider {
	return gemini.NewCompletionProvider(apiKey, timeout)
}

// OpenAICodex creates a CompletionProvider using ChatGPT subscription via OAuth.
// refreshToken is the initial token from env; tokenFile persists rotated tokens.
func OpenAICodex(refreshToken, tokenFile string, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) CompletionProvider {
	ts := openaicodex.NewTokenStore(tokenFile, logger)
	if err := ts.Load(); err != nil {
		logger.Error("openai-codex: load tokens", "error", err)
	}
	ts.Bootstrap(refreshToken)
	return openaicodex.NewCompletionProvider(ts, timeout, backoffs, logger)
}

// AnthropicTokenStore is the rotating OAuth token pair behind an AnthropicOAuth
// provider. Re-exported so a long-lived host can drive refresh off the request
// path and report token health; the request path refreshes on its own either way.
type AnthropicTokenStore = anthropicoauth.TokenStore

// AnthropicTokenStatus is a point-in-time view of token health.
type AnthropicTokenStatus = anthropicoauth.Status

// AnthropicOAuth creates a CompletionProvider using Claude subscription via OAuth.
// refreshToken is the initial token from env (minted by the host OAuth login flow);
// tokenFile persists rotated tokens. Requests are made through the standard
// Anthropic Messages API but authenticated with a subscription-billed bearer
// token instead of an API key — usage counts against the Claude Code plan.
func AnthropicOAuth(refreshToken, tokenFile string, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) CompletionProvider {
	provider, _ := AnthropicOAuthWithTokens(refreshToken, tokenFile, timeout, backoffs, logger)
	return provider
}

// AnthropicOAuthWithTokens is AnthropicOAuth plus the token store, for hosts
// that outlive a single request and want to refresh ahead of expiry rather
// than on the first request that finds the token stale.
func AnthropicOAuthWithTokens(refreshToken, tokenFile string, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) (CompletionProvider, *AnthropicTokenStore) {
	ts := anthropicoauth.NewTokenStore(tokenFile, logger)
	if err := ts.Load(); err != nil {
		logger.Error("anthropic-oauth: load tokens", "error", err)
	}
	ts.Bootstrap(refreshToken)
	return anthropic.NewOAuthProvider(ts, timeout, backoffs, logger), ts
}

// Telegram creates a TransportConfig for Telegram.
func Telegram(botToken string) TransportConfig {
	return TransportConfig{
		Type:     "telegram",
		BotToken: botToken,
	}
}

// OpenAIEmbedding creates an EmbeddingProvider using OpenAI embeddings.
func OpenAIEmbedding(apiKey string) EmbeddingProvider {
	return openai.NewEmbeddingProvider(apiKey, "text-embedding-3-small", 15*time.Second)
}

// OpenAIEmbeddingWithModel creates an EmbeddingProvider with a custom model.
func OpenAIEmbeddingWithModel(apiKey, model string, timeout time.Duration) EmbeddingProvider {
	return openai.NewEmbeddingProvider(apiKey, model, timeout)
}

// GeminiEmbeddingWithModel creates an EmbeddingProvider over the Gemini
// embedding API (e.g. gemini-embedding-001) at the given width (0 = the
// model's native width). It also implements QueryEmbedder: documents and
// queries are encoded with their own task types.
func GeminiEmbeddingWithModel(apiKey, model string, dimension int, timeout time.Duration) EmbeddingProvider {
	return gemini.NewEmbeddingProvider(apiKey, model, dimension, timeout)
}

// QueryEmbedder is re-exported for hosts that embed search queries.
type QueryEmbedder = core.QueryEmbedder

// Serper creates a SearchEngine using the Serper.dev Google Search API.
func Serper(apiKey string) SearchEngine {
	return web.NewSerperSearch(apiKey)
}

// HTTPFetcher creates a WebFetcher that downloads and extracts text from web pages.
func HTTPFetcher() WebFetcher {
	return web.NewHTTPFetcher()
}

// Whisper creates a TranscriptionProvider using OpenAI Whisper.
func Whisper(apiKey string) TranscriptionProvider {
	return openai.NewTranscriptionProvider(apiKey, "whisper-1", 30*time.Second)
}

// WhisperWithModel creates a TranscriptionProvider with a custom model.
func WhisperWithModel(apiKey, model string, timeout time.Duration) TranscriptionProvider {
	return openai.NewTranscriptionProvider(apiKey, model, timeout)
}

// WhisperLocal creates a TranscriptionProvider pointing to a local OpenAI-compatible
// STT endpoint (e.g. MLX Whisper on localhost).
func WhisperLocal(endpoint, model string, timeout time.Duration) TranscriptionProvider {
	return openai.NewTranscriptionProviderWithEndpoint(endpoint, model, timeout)
}

// TelegramSender creates a MessageSender using the Telegram Bot API.
func TelegramSender(botToken string, timeout time.Duration) MessageSender {
	return &telegramSenderAdapter{client: telegram.NewClient(botToken, timeout)}
}

// TelegramSenderWithAPIURL creates a MessageSender backed by a custom Bot API endpoint.
func TelegramSenderWithAPIURL(botToken, apiURL string, timeout time.Duration) MessageSender {
	return &telegramSenderAdapter{client: telegram.NewClientWithAPIURL(botToken, apiURL, timeout)}
}

// telegramSenderAdapter wraps telegram.Client to satisfy MessageSender.
type telegramSenderAdapter struct {
	client *telegram.Client
}

func (a *telegramSenderAdapter) SendMessage(ctx context.Context, chatID string, text string) (int, error) {
	if id, parseErr := strconv.ParseInt(chatID, 10, 64); parseErr == nil {
		if result, richErr := a.client.SendRichMessage(ctx, id, text); richErr == nil {
			return result.Result.MessageID, nil
		}
	}
	result, err := a.client.SendMessage(ctx, chatID, text)
	if err != nil {
		return 0, err
	}
	return result.Result.MessageID, nil
}

func (a *telegramSenderAdapter) SendLong(ctx context.Context, chatID string, text string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		_, err = a.client.SendMessage(ctx, chatID, text)
		return err
	}
	return a.client.SendRichLong(ctx, id, text)
}

func (a *telegramSenderAdapter) SendVoice(ctx context.Context, chatID string, audio []byte) error {
	return a.client.SendVoice(ctx, chatID, audio)
}
