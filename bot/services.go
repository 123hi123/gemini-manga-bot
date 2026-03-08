package bot

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"tg-bawer/gemini"
	"tg-bawer/grok"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func (b *Bot) cmdService(msg *tgbotapi.Message) {
	args := strings.Fields(msg.CommandArguments())
	if len(args) == 0 {
		b.sendServiceHelp(msg)
		return
	}

	switch strings.ToLower(args[0]) {
	case "help":
		b.sendServiceHelp(msg)
	case "list":
		b.sendServiceList(msg)
	case "add":
		b.cmdServiceAdd(msg, args)
	case "use":
		b.cmdServiceUse(msg, args)
	case "delete", "del", "rm":
		b.cmdServiceDelete(msg, args)
	default:
		b.sendServiceHelp(msg)
	}
}

func (b *Bot) sendServiceHelp(msg *tgbotapi.Message) {
	helpText := `🔌 *服務管理*

你可以新增四種服務來源：
1) ` + "`standard`" + `：只填 API Key（官方 Gemini）
2) ` + "`custom`" + `：自訂 Base URL + API Key
	3) ` + "`vertex`" + `：Vertex（支援只填 API Key 的 express mode）
4) ` + "`grok`" + `：Grok 影像生成（自訂 Base URL + API Key）

*指令格式：*
` + "`/service list`" + `
` + "`/service use <服務ID>`" + `
` + "`/service delete <服務ID>`" + `

` + "`/service add standard <名稱> <API_KEY>`" + `
` + "`/service add custom <名稱> <BASE_URL> <API_KEY>`" + `
	` + "`/service add vertex <名稱> <API_KEY>`" + `  (express mode)
	` + "`/service add vertex <名稱> <API_KEY> <PROJECT_ID> <LOCATION> [MODEL] [BASE_URL]`" + `  (full mode)
` + "`/service add grok <名稱> <BASE_URL> <API_KEY>`" + `
` + "`/service add grok custom <名稱> <BASE_URL> <API_KEY>`" + `

*範例：*
` + "`/service add standard my-gemini AIza...`" + `
` + "`/service add custom my-proxy https://your-proxy.example.com AIza...`" + `
	` + "`/service add vertex my-vertex AIza...`" + `
	` + "`/service add vertex my-vertex AIza... my-project asia-east1 gemini-3-pro-image-preview`" + `
` + "`/service add grok my-grok http://your-grok-host:8000 sk-...`" + `
` + "`/service add grok custom 66 http://48.218.144.171:53768 sk-...`"

	reply := tgbotapi.NewMessage(msg.Chat.ID, helpText)
	reply.ParseMode = "Markdown"
	b.api.Send(reply)
}

func (b *Bot) sendServiceList(msg *tgbotapi.Message) {
	services, err := b.db.GetUserServices(msg.From.ID)
	if err != nil {
		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 讀取服務列表失敗："+err.Error()))
		return
	}

	var lines []string
	lines = append(lines, "🔌 你的服務列表：")

	for _, service := range services {
		defaultMark := ""
		if service.IsDefault {
			defaultMark = " [預設]"
		}

		detail := fmt.Sprintf(
			"#%d %s (%s)%s key=%s",
			service.ID,
			service.Name,
			service.Type,
			defaultMark,
			maskSecret(service.APIKey),
		)

		if (service.Type == gemini.ServiceTypeCustom || service.Type == grok.ServiceTypeGrok) && service.BaseURL != "" {
			detail += " base=" + service.BaseURL
		}

		if service.Type == gemini.ServiceTypeVertex {
			if service.ProjectID != "" && service.Location != "" {
				detail += fmt.Sprintf(" project=%s location=%s", service.ProjectID, service.Location)
			} else {
				detail += " mode=express"
			}
			if service.Model != "" {
				detail += " model=" + service.Model
			}
			if service.BaseURL != "" {
				detail += " base=" + service.BaseURL
			}
		}

		lines = append(lines, detail)
	}

	if len(services) == 0 {
		lines = append(lines, "（尚未新增服務）")
	}

	if strings.TrimSpace(b.config.GeminiAPIKey) != "" {
		lines = append(lines, "ENV fallback: GEMINI_API_KEY 已設定")
	}

	lines = append(lines, "")
	lines = append(lines, "用 /service help 查看新增格式")

	b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, strings.Join(lines, "\n")))
}

func (b *Bot) cmdServiceAdd(msg *tgbotapi.Message, args []string) {
	if len(args) < 2 {
		b.sendServiceHelp(msg)
		return
	}

	mode := strings.ToLower(args[1])
	switch mode {
	case "standard", "gemini", "origin", "original":
		if len(args) < 4 {
			b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 格式：/service add standard <名稱> <API_KEY>"))
			return
		}

		id, err := b.db.AddUserService(
			msg.From.ID,
			gemini.ServiceTypeStandard,
			args[2],
			args[3],
			"",
			"",
			"",
			"",
			true,
		)
		if err != nil {
			b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 新增 standard 服務失敗："+err.Error()))
			return
		}

		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("✅ 已新增 standard 服務 #%d，並設為預設", id)))

	case "custom":
		if len(args) < 5 {
			b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 格式：/service add custom <名稱> <BASE_URL> <API_KEY>"))
			return
		}

		id, err := b.db.AddUserService(
			msg.From.ID,
			gemini.ServiceTypeCustom,
			args[2],
			args[4],
			args[3],
			"",
			"",
			"",
			true,
		)
		if err != nil {
			b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 新增 custom 服務失敗："+err.Error()))
			return
		}

		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("✅ 已新增 custom 服務 #%d，並設為預設", id)))

	case "vertex":
		// 支援兩種格式：
		// 1) express mode: /service add vertex <名稱> <API_KEY>
		// 2) full mode:    /service add vertex <名稱> <API_KEY> <PROJECT_ID> <LOCATION> [MODEL] [BASE_URL]
		if len(args) < 4 {
			b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 格式：/service add vertex <名稱> <API_KEY> 或 /service add vertex <名稱> <API_KEY> <PROJECT_ID> <LOCATION> [MODEL] [BASE_URL]"))
			return
		}

		projectID := ""
		location := ""
		model := ""
		baseURL := ""

		if len(args) >= 6 {
			projectID = args[4]
			location = args[5]
		}
		if len(args) >= 7 {
			model = args[6]
		}
		if len(args) >= 8 {
			baseURL = args[7]
		}

		id, err := b.db.AddUserService(
			msg.From.ID,
			gemini.ServiceTypeVertex,
			args[2],
			args[3],
			baseURL,
			projectID,
			location,
			model,
			true,
		)
		if err != nil {
			b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 新增 vertex 服務失敗："+err.Error()))
			return
		}

		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("✅ 已新增 vertex 服務 #%d，並設為預設", id)))

	case "grok":
		// Formats:
		// 1) /service add grok <名稱> <BASE_URL> <API_KEY>
		// 2) /service add grok custom <名稱> <BASE_URL> <API_KEY>
		var name, baseURL, apiKey string
		if len(args) >= 3 && strings.ToLower(args[2]) == "custom" {
			// explicit "custom" sub-type keyword
			if len(args) < 6 {
				b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 格式：/service add grok custom <名稱> <BASE_URL> <API_KEY>"))
				return
			}
			name = args[3]
			baseURL = args[4]
			apiKey = args[5]
		} else {
			if len(args) < 5 {
				b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 格式：/service add grok <名稱> <BASE_URL> <API_KEY>"))
				return
			}
			name = args[2]
			baseURL = args[3]
			apiKey = args[4]
		}

		id, err := b.db.AddUserService(
			msg.From.ID,
			grok.ServiceTypeGrok,
			name,
			apiKey,
			baseURL,
			"",
			"",
			"",
			true,
		)
		if err != nil {
			b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 新增 grok 服務失敗："+err.Error()))
			return
		}

		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("✅ 已新增 grok 服務 #%d，並設為預設", id)))

	default:
		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 不支援的服務類型，請用 standard/custom/vertex/grok"))
	}
}

func (b *Bot) cmdServiceUse(msg *tgbotapi.Message, args []string) {
	if len(args) < 2 {
		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 格式：/service use <服務ID>"))
		return
	}

	serviceID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 服務 ID 必須是數字"))
		return
	}

	if err := b.db.SetDefaultUserService(msg.From.ID, serviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 找不到該服務 ID，請先用 /service list 查詢"))
			return
		}
		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 設定預設服務失敗："+err.Error()))
		return
	}

	b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("✅ 已切換預設服務為 #%d", serviceID)))
}

func (b *Bot) cmdServiceDelete(msg *tgbotapi.Message, args []string) {
	if len(args) < 2 {
		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 格式：/service delete <服務ID>"))
		return
	}

	serviceID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 服務 ID 必須是數字"))
		return
	}

	if err := b.db.DeleteUserService(msg.From.ID, serviceID); err != nil {
		b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ 刪除服務失敗："+err.Error()))
		return
	}

	b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("✅ 已刪除服務 #%d", serviceID)))
}

// resolveAllServiceConfigs returns all available Gemini/Google service configs for a user (for rotation).
// Grok services are excluded here; use resolveGrokClient for those.
func (b *Bot) resolveAllServiceConfigs(userID int64) ([]gemini.ServiceConfig, error) {
	services, err := b.db.GetAllUserServices(userID)
	if err != nil {
		return nil, err
	}

	var configs []gemini.ServiceConfig
	for _, service := range services {
		if service.Type == grok.ServiceTypeGrok {
			continue // grok services are handled separately
		}
		configs = append(configs, gemini.ServiceConfig{
			Type:      service.Type,
			Name:      service.Name,
			APIKey:    service.APIKey,
			BaseURL:   service.BaseURL,
			ProjectID: service.ProjectID,
			Location:  service.Location,
			Model:     service.Model,
		})
	}

	if len(configs) == 0 && strings.TrimSpace(b.config.GeminiAPIKey) != "" {
		configs = append(configs, gemini.ServiceConfig{
			Type:    gemini.ServiceTypeStandard,
			Name:    "env-default",
			APIKey:  b.config.GeminiAPIKey,
			BaseURL: b.config.GeminiBaseURL,
			Model:   b.config.GeminiModel,
		})
	}

	return configs, nil
}

// resolveGrokClient returns a Grok client for the given user.
// It first checks the user's DB-configured Grok services (default first), then falls back to
// the environment-variable-configured client. Returns nil if no Grok service is available.
func (b *Bot) resolveGrokClient(userID int64) *grok.Client {
	services, err := b.db.GetAllUserServices(userID)
	if err == nil {
		for _, s := range services {
			if s.Type == grok.ServiceTypeGrok {
				// Empty strings for imgModel/editModel/videoModel use the package defaults.
				return grok.NewClient(s.APIKey, s.BaseURL, "", "", "")
			}
		}
	}
	if b.grokClient.Available() {
		return b.grokClient
	}
	return nil
}

func (b *Bot) resolveServiceConfig(userID int64) (gemini.ServiceConfig, string, error) {
	services, err := b.resolveAllServiceConfigs(userID)
	if err != nil {
		return gemini.ServiceConfig{}, "", err
	}

	if len(services) > 0 {
		return services[0], services[0].Name, nil
	}

	return gemini.ServiceConfig{}, "", fmt.Errorf("尚未設定服務，請先使用 /service add")
}

func maskSecret(secret string) string {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return "(empty)"
	}
	if len(trimmed) <= 8 {
		return "****"
	}
	return trimmed[:4] + "..." + trimmed[len(trimmed)-4:]
}
