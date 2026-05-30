package blueship

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/rasimio/blueship/internal/provider/anthropic"
	"github.com/rasimio/blueship/internal/provider/anthropicoauth"
	"github.com/rasimio/blueship/internal/provider/gemini"
	"github.com/rasimio/blueship/internal/provider/ollama"
	"github.com/rasimio/blueship/internal/provider/openai"
	"github.com/rasimio/blueship/internal/provider/openaicodex"
	"github.com/rasimio/blueship/internal/transport/telegram"
	"github.com/rasimio/blueship/internal/web"
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
func Ollama(baseURL string, timeout time.Duration) CompletionProvider {
	return ollama.NewCompletionProvider(baseURL, timeout)
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

// AnthropicOAuth creates a CompletionProvider using Claude subscription via OAuth.
// refreshToken is the initial token from env (minted by `arlene-anthropic-login`);
// tokenFile persists rotated tokens. Requests are made through the standard
// Anthropic Messages API but authenticated with a subscription-billed bearer
// token instead of an API key — usage counts against the Claude Code plan.
func AnthropicOAuth(refreshToken, tokenFile string, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) CompletionProvider {
	ts := anthropicoauth.NewTokenStore(tokenFile, logger)
	if err := ts.Load(); err != nil {
		logger.Error("anthropic-oauth: load tokens", "error", err)
	}
	ts.Bootstrap(refreshToken)
	return anthropic.NewOAuthProvider(ts.AccessToken, timeout, backoffs, logger)
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

// Serper creates a SearchEngine using the Serper.dev Google Search API.
func Serper(apiKey string) SearchEngine {
	return web.NewSerperSearch(apiKey)
}

// NewHTTPFetcher creates a WebFetcher that downloads and extracts text from web pages.
func NewHTTPFetcher() WebFetcher {
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

// telegramSenderAdapter wraps telegram.Client to satisfy MessageSender.
type telegramSenderAdapter struct {
	client *telegram.Client
}

func (a *telegramSenderAdapter) SendMessage(ctx context.Context, chatID string, text string) (int, error) {
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
	return a.client.SendLong(ctx, id, text)
}

func (a *telegramSenderAdapter) SendVoice(ctx context.Context, chatID string, audio []byte) error {
	return a.client.SendVoice(ctx, chatID, audio)
}
