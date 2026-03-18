package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rasimio/blueship/internal/telegram"
)

// Available models grouped by provider.
var availableModels = map[string][]string{
	"gemini": {
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"gemini-3.1-pro-preview",
	},
	"anthropic": {
		"claude-sonnet-4-6",
		"claude-opus-4-6",
		"claude-haiku-4-5-20251001",
	},
	"openai": {
		"gpt-4o",
		"gpt-4o-mini",
	},
}

// handleModelCommand sends inline keyboard with role selection.
func (g *Gateway) handleModelCommand(ctx context.Context, chatID int64) {
	if g.deps.ModelStore == nil {
		g.tg.SendLong(ctx, chatID, "Model config not available.")
		return
	}

	_ = g.deps.ModelStore.Refresh(ctx)

	roles := g.deps.ModelStore.Roles()
	sort.Strings(roles)

	var rows [][]telegram.InlineKeyboardButton
	for _, role := range roles {
		ref := g.deps.ModelStore.Get(role)
		label := fmt.Sprintf("%s: %s:%s", role, ref.Provider, ref.Name)
		rows = append(rows, []telegram.InlineKeyboardButton{
			{Text: label, CallbackData: "model_role:" + role},
		})
	}

	g.tg.SendMessageWithKeyboard(ctx, chatID, "Select role to change:", rows)
}

// handleModelCallback processes callback_query data for /model flow.
// Returns true if the callback was handled.
func (g *Gateway) handleModelCallback(ctx context.Context, cq *telegram.CallbackQuery) bool {
	data := cq.Data
	chatID := cq.From.ID

	if strings.HasPrefix(data, "model_role:") {
		role := strings.TrimPrefix(data, "model_role:")
		g.showModelPicker(ctx, chatID, cq.Message.MessageID, role)
		return true
	}

	if strings.HasPrefix(data, "model_set:") {
		// format: model_set:role:provider:model_name
		parts := strings.SplitN(strings.TrimPrefix(data, "model_set:"), ":", 3)
		if len(parts) == 3 {
			g.setModel(ctx, chatID, cq.Message.MessageID, parts[0], parts[1], parts[2])
		}
		return true
	}

	if data == "model_back" {
		g.showRolePicker(ctx, chatID, cq.Message.MessageID)
		return true
	}

	return false
}

func (g *Gateway) showRolePicker(ctx context.Context, chatID int64, messageID int) {
	if g.deps.ModelStore == nil {
		return
	}
	roles := g.deps.ModelStore.Roles()
	sort.Strings(roles)

	var rows [][]telegram.InlineKeyboardButton
	for _, role := range roles {
		ref := g.deps.ModelStore.Get(role)
		label := fmt.Sprintf("%s: %s:%s", role, ref.Provider, ref.Name)
		rows = append(rows, []telegram.InlineKeyboardButton{
			{Text: label, CallbackData: "model_role:" + role},
		})
	}

	g.tg.EditMessageText(ctx, chatID, messageID, "Select role to change:", rows)
}

func (g *Gateway) showModelPicker(ctx context.Context, chatID int64, messageID int, role string) {
	var rows [][]telegram.InlineKeyboardButton

	current := g.deps.ModelStore.Get(role)
	currentKey := current.Provider + ":" + current.Name

	// Sort providers for consistent order
	providers := make([]string, 0, len(availableModels))
	for p := range availableModels {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	for _, provider := range providers {
		models := availableModels[provider]
		for _, model := range models {
			key := provider + ":" + model
			label := key
			if key == currentKey {
				label = ">> " + label + " <<"
			}
			cbData := fmt.Sprintf("model_set:%s:%s:%s", role, provider, model)
			// Telegram callback_data max 64 bytes — truncate if needed
			if len(cbData) > 64 {
				cbData = cbData[:64]
			}
			rows = append(rows, []telegram.InlineKeyboardButton{
				{Text: label, CallbackData: cbData},
			})
		}
	}

	// Back button
	rows = append(rows, []telegram.InlineKeyboardButton{
		{Text: "<< back", CallbackData: "model_back"},
	})

	text := fmt.Sprintf("Select model for <b>%s</b>\n(current: %s)", role, currentKey)
	g.tg.EditMessageText(ctx, chatID, messageID, text, rows)
}

func (g *Gateway) setModel(ctx context.Context, chatID int64, messageID int, role, provider, modelName string) {
	if g.deps.ModelStore == nil {
		return
	}

	if err := g.deps.ModelStore.Update(ctx, role, provider, modelName); err != nil {
		g.logger.Error("model update failed", "role", role, "error", err)
		g.tg.EditMessageText(ctx, chatID, messageID, fmt.Sprintf("Failed to update %s: %v", role, err), nil)
		return
	}

	g.logger.Info("model updated via /model command",
		"role", role,
		"provider", provider,
		"model", modelName,
	)

	text := fmt.Sprintf("%s -> %s:%s", role, provider, modelName)
	g.tg.EditMessageText(ctx, chatID, messageID, text, nil)
}
