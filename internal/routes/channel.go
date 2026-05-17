package routes

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/utils"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gotd/td/tg"
)

// MessageItem holds all raw data for a single channel message
type MessageItem struct {
	ID          int
	Caption     string
	Date        string
	MediaType   string
	FileName    string
	MimeType    string
	FileSize    int64
	StreamURL   string
	DownloadURL string
	Views       int
	HasMedia    bool
	Thumbnail   string
}

func (e *allRoutes) LoadChannel(r *Route) {
	log = e.log.Named("Channel")
	defer log.Info("Loaded channel route")
	r.Engine.GET("/channel", getChannelRoute)
}

func getChannelRoute(ctx *gin.Context) {
	// Use main Bot client — no USER_SESSION needed.
	// Bot must be admin/member of the log channel.
	botClient := bot.Bot
	if botClient == nil {
		http.Error(ctx.Writer, "Bot client is not running.", http.StatusServiceUnavailable)
		return
	}

	// Query params:
	//   ?msg_id=N  → fetch messages starting from ID N (inclusive), ascending
	//   ?limit=N   → how many to fetch (default 100, max 200)
	startMsgID := 0
	if v, err := strconv.Atoi(ctx.Query("msg_id")); err == nil && v > 0 {
		startMsgID = v
	}
	limit := 100
	if v, err := strconv.Atoi(ctx.Query("limit")); err == nil && v > 0 {
		if v > 200 {
			v = 200
		}
		limit = v
	}

	// Resolve channel peer using bot's peer storage
	// channel_id query param lets you inspect any course channel.
	// Falls back to LOG_CHANNEL if not provided.
	var targetChannelID int64
	if v := ctx.Query("channel_id"); v != "" {
		if parsed, err2 := strconv.ParseInt(v, 10, 64); err2 == nil {
			targetChannelID = parsed
		}
	}
	var channelPeer *tg.InputChannel
	var err error
	if targetChannelID != 0 {
		channelPeer, err = utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, targetChannelID)
	} else {
		channelPeer, err = utils.GetLogChannelPeer(ctx, botClient.API(), botClient.PeerStorage)
	}
	if err != nil {
		http.Error(ctx.Writer, "Failed to resolve channel: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var rawMessages []tg.MessageClass

	if startMsgID == 0 {
		// Fetch latest messages via getHistory
		history, err := botClient.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:      &tg.InputPeerChannel{ChannelID: channelPeer.ChannelID, AccessHash: channelPeer.AccessHash},
			Limit:     limit,
			OffsetID:  0,
			AddOffset: 0,
			MaxID:     0,
			MinID:     0,
		})
		if err != nil {
			http.Error(ctx.Writer,
				"Bot cannot fetch channel history without a starting msg_id on this layer. "+
					"Use /channel?msg_id=1&limit=100 to fetch from message ID 1.",
				http.StatusBadRequest)
			return
		}
		switch h := history.(type) {
		case *tg.MessagesChannelMessages:
			rawMessages = h.Messages
		case *tg.MessagesMessages:
			rawMessages = h.Messages
		case *tg.MessagesMessagesSlice:
			rawMessages = h.Messages
		default:
			http.Error(ctx.Writer, "Unexpected history type", http.StatusInternalServerError)
			return
		}
	} else {
		// Build explicit ID list: startMsgID, startMsgID+1, ..., startMsgID+limit-1
		ids := make([]tg.InputMessageClass, 0, limit)
		for i := 0; i < limit; i++ {
			ids = append(ids, &tg.InputMessageID{ID: startMsgID + i})
		}
		res, err := botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: channelPeer,
			ID:      ids,
		})
		if err != nil {
			http.Error(ctx.Writer, "Failed to fetch messages: "+err.Error(), http.StatusInternalServerError)
			return
		}
		switch h := res.(type) {
		case *tg.MessagesChannelMessages:
			rawMessages = h.Messages
		default:
			http.Error(ctx.Writer, "Unexpected response type", http.StatusInternalServerError)
			return
		}
	}

	host := config.ValueOf.Host
	var items []MessageItem

	for _, raw := range rawMessages {
		msg, ok := raw.(*tg.Message)
		if !ok {
			continue
		}

		item := MessageItem{
			ID:       msg.ID,
			Caption:  msg.Message,
			Date:     time.Unix(int64(msg.Date), 0).UTC().Format("02 Jan 2006 • 15:04 UTC"),
			Views:    msg.Views,
			HasMedia: msg.Media != nil,
		}

		if msg.Media != nil {
			file, err := utils.FileFromMedia(msg.Media)
			if err == nil {
				item.FileName = file.FileName
				item.MimeType = file.MimeType
				item.FileSize = file.FileSize
				item.StreamURL = fmt.Sprintf("%s/stream/%d", host, msg.ID)
				item.DownloadURL = fmt.Sprintf("%s/stream/%d?d=true", host, msg.ID)
				mime := strings.ToLower(file.MimeType)
				switch {
				case strings.Contains(mime, "video"):
					item.MediaType = "video"
				case strings.Contains(mime, "audio"):
					item.MediaType = "audio"
				case mime == "application/pdf":
					item.MediaType = "pdf"
				case strings.Contains(mime, "image"):
					item.MediaType = "photo"
					item.Thumbnail = item.StreamURL
				default:
					item.MediaType = "document"
				}
			} else {
				if _, ok := msg.Media.(*tg.MessageMediaPhoto); ok {
					item.MediaType = "photo"
					item.MimeType = "image/jpeg"
					item.StreamURL = fmt.Sprintf("%s/stream/%d", host, msg.ID)
					item.DownloadURL = fmt.Sprintf("%s/stream/%d?d=true", host, msg.ID)
					item.Thumbnail = item.StreamURL
				}
			}
		} else {
			item.MediaType = "none"
		}

		items = append(items, item)
	}

	// Sort ascending: oldest message first
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})

	// Pagination helpers
	nextMsgID := 0
	if len(items) > 0 {
		nextMsgID = items[len(items)-1].ID + 1
	}
	prevMsgID := startMsgID - limit
	if prevMsgID < 1 {
		prevMsgID = 1
	}

	tmpl, err := template.New("channel").Funcs(template.FuncMap{
		"formatSize": func(b int64) string {
			if b == 0 {
				return ""
			}
			const unit = 1024
			if b < unit {
				return fmt.Sprintf("%d B", b)
			}
			div, exp := int64(unit), 0
			for n := b / unit; n >= unit; n /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
		},
		"hasPrefix": strings.HasPrefix,
		"add": func(a, b int) int { return a + b },
	}).Parse(channelPageTemplate)
	if err != nil {
		http.Error(ctx.Writer, "Template error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ctx.Header("Content-Type", "text/html; charset=utf-8")
	data := struct {
		Items      []MessageItem
		ChannelID  int64
		TotalCount int
		StartMsgID int
		NextMsgID  int
		PrevMsgID  int
		Limit      int
	}{
		Items:      items,
		ChannelID:  channelPeer.ChannelID,
		TotalCount: len(items),
		StartMsgID: startMsgID,
		NextMsgID:  nextMsgID,
		PrevMsgID:  prevMsgID,
		Limit:      limit,
	}
	if err := tmpl.Execute(ctx.Writer, data); err != nil {
		log.Sugar().Errorf("Template execute error: %v", err)
	}
}

const channelPageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
<title>📁 Channel Files</title>
<style>
  :root {
    --bg:#0f0f13;--surface:#1a1a24;--card:#22222e;--border:#2e2e40;
    --accent:#7c6af7;--accent2:#5eead4;--text:#e8e8f0;--muted:#7070a0;
    --video:#7c6af7;--audio:#f472b6;--pdf:#fb923c;--photo:#34d399;--doc:#60a5fa;--none:#6b7280;
    --radius:14px;--shadow:0 4px 24px rgba(0,0,0,.4);
  }
  *{box-sizing:border-box;margin:0;padding:0}
  body{background:var(--bg);color:var(--text);font-family:'Segoe UI',system-ui,sans-serif;min-height:100vh}
  header{background:linear-gradient(135deg,#1a1030,#0f1a2e);border-bottom:1px solid var(--border);padding:28px 24px 20px;text-align:center;position:sticky;top:0;z-index:100;backdrop-filter:blur(12px)}
  header h1{font-size:1.7rem;font-weight:700;background:linear-gradient(90deg,var(--accent),var(--accent2));-webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text}
  header p{color:var(--muted);font-size:.85rem;margin-top:4px}
  .page-info{display:flex;align-items:center;justify-content:center;gap:12px;flex-wrap:wrap;padding:10px 16px 0;font-size:.82rem;color:var(--muted)}
  .page-info strong{color:var(--text)}
  .url-hint{background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:8px 14px;font-size:.78rem;color:var(--muted);font-family:monospace;margin:10px auto 0;max-width:560px;text-align:center}
  .url-hint a{color:var(--accent);text-decoration:none}
  .url-hint a:hover{text-decoration:underline}
  .filters{display:flex;gap:8px;flex-wrap:wrap;justify-content:center;padding:16px 16px 4px}
  .filter-btn{padding:6px 16px;border-radius:999px;border:1px solid var(--border);background:var(--surface);color:var(--muted);cursor:pointer;font-size:.8rem;font-weight:600;transition:all .2s}
  .filter-btn:hover,.filter-btn.active{background:var(--accent);color:#fff;border-color:var(--accent)}
  .filter-btn[data-type="video"].active{background:var(--video);border-color:var(--video)}
  .filter-btn[data-type="audio"].active{background:var(--audio);border-color:var(--audio)}
  .filter-btn[data-type="pdf"].active{background:var(--pdf);border-color:var(--pdf)}
  .filter-btn[data-type="photo"].active{background:var(--photo);border-color:var(--photo)}
  .filter-btn[data-type="document"].active{background:var(--doc);border-color:var(--doc)}
  .search-wrap{max-width:440px;margin:12px auto 0;position:relative}
  .search-wrap input{width:100%;background:var(--surface);border:1px solid var(--border);color:var(--text);border-radius:999px;padding:8px 16px 8px 38px;font-size:.85rem;outline:none;transition:border-color .2s}
  .search-wrap input:focus{border-color:var(--accent)}
  .search-wrap .icon{position:absolute;left:13px;top:50%;transform:translateY(-50%);color:var(--muted)}
  .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,1fr));gap:18px;padding:20px 20px 20px;max-width:1300px;margin:0 auto}
  .card{background:var(--card);border:1px solid var(--border);border-radius:var(--radius);overflow:hidden;display:flex;flex-direction:column;box-shadow:var(--shadow);transition:transform .2s,box-shadow .2s}
  .card:hover{transform:translateY(-3px);box-shadow:0 8px 32px rgba(0,0,0,.6)}
  .card-stripe{height:4px;width:100%}
  .type-video .card-stripe{background:linear-gradient(90deg,var(--video),#b48ef7)}
  .type-audio .card-stripe{background:linear-gradient(90deg,var(--audio),#f9a8d4)}
  .type-pdf .card-stripe{background:linear-gradient(90deg,var(--pdf),#fcd34d)}
  .type-photo .card-stripe{background:linear-gradient(90deg,var(--photo),#6ee7b7)}
  .type-document .card-stripe{background:linear-gradient(90deg,var(--doc),#93c5fd)}
  .type-none .card-stripe{background:var(--border)}
  .card-body{padding:16px 16px 12px;flex:1;display:flex;flex-direction:column;gap:10px}
  .card-header-row{display:flex;align-items:center;justify-content:space-between}
  .type-badge{display:inline-flex;align-items:center;gap:5px;font-size:.72rem;font-weight:700;letter-spacing:.5px;text-transform:uppercase;padding:3px 10px;border-radius:999px;border:1px solid}
  .type-video .type-badge{color:var(--video);border-color:var(--video);background:#7c6af715}
  .type-audio .type-badge{color:var(--audio);border-color:var(--audio);background:#f472b615}
  .type-pdf .type-badge{color:var(--pdf);border-color:var(--pdf);background:#fb923c15}
  .type-photo .type-badge{color:var(--photo);border-color:var(--photo);background:#34d39915}
  .type-document .type-badge{color:var(--doc);border-color:var(--doc);background:#60a5fa15}
  .type-none .type-badge{color:var(--none);border-color:var(--none);background:#6b728015}
  .msg-id{font-size:.72rem;color:var(--muted);font-family:monospace}
  .file-name{font-size:.92rem;font-weight:600;word-break:break-word;line-height:1.35}
  .caption{font-size:.82rem;color:var(--muted);line-height:1.5;word-break:break-word;white-space:pre-wrap;max-height:90px;overflow:hidden;position:relative}
  .caption::after{content:'';position:absolute;bottom:0;left:0;right:0;height:28px;background:linear-gradient(transparent,var(--card));pointer-events:none}
  .meta{display:flex;gap:12px;font-size:.75rem;color:var(--muted);flex-wrap:wrap}
  .meta span{display:flex;align-items:center;gap:4px}
  .card-actions{padding:0 16px 14px;display:flex;gap:8px;flex-wrap:wrap}
  .btn{display:inline-flex;align-items:center;gap:6px;padding:7px 16px;border-radius:8px;font-size:.8rem;font-weight:600;text-decoration:none;transition:opacity .2s,transform .15s;cursor:pointer}
  .btn:hover{opacity:.88;transform:scale(.98)}
  .btn-watch{background:var(--video);color:#fff}
  .btn-listen{background:var(--audio);color:#fff}
  .btn-view{background:var(--pdf);color:#fff}
  .btn-photo{background:var(--photo);color:#111}
  .btn-open{background:var(--doc);color:#fff}
  .btn-dl{background:transparent;color:var(--muted);border:1px solid var(--border)}
  .btn-dl:hover{color:var(--text);border-color:var(--text)}
  .pagination{display:flex;justify-content:center;gap:12px;padding:16px 20px 40px;flex-wrap:wrap}
  .pagination a{display:inline-flex;align-items:center;gap:6px;padding:9px 22px;border-radius:10px;background:var(--surface);color:var(--text);border:1px solid var(--border);text-decoration:none;font-size:.85rem;font-weight:600;transition:all .2s}
  .pagination a:hover{background:var(--accent);border-color:var(--accent);color:#fff}
  .empty{grid-column:1/-1;text-align:center;padding:80px 20px;color:var(--muted)}
  .empty .emoji{font-size:3rem;display:block;margin-bottom:12px}
  ::-webkit-scrollbar{width:6px}
  ::-webkit-scrollbar-track{background:var(--bg)}
  ::-webkit-scrollbar-thumb{background:var(--border);border-radius:4px}
  @media(max-width:480px){.grid{padding:12px 12px 16px;gap:12px}header h1{font-size:1.3rem}}
</style>
</head>
<body>

<header>
  <h1>📁 Channel Files</h1>
  <p>{{.TotalCount}} messages • ascending order (oldest → newest)</p>
  <div class="search-wrap">
    <span class="icon">🔍</span>
    <input type="text" id="searchInput" placeholder="Search files or captions…" oninput="applyFilters()"/>
  </div>
</header>

{{if gt .StartMsgID 0}}
<div class="page-info">
  Showing msg IDs <strong>#{{.StartMsgID}}</strong> → <strong>#{{add .NextMsgID -1}}</strong>
  &nbsp;|&nbsp; limit: <strong>{{.Limit}}</strong>
</div>
{{else}}
<div class="page-info">Showing latest <strong>{{.Limit}}</strong> messages</div>
{{end}}

<div class="url-hint">
  From start: <a href="/channel?msg_id=1&limit={{.Limit}}">/channel?msg_id=1&amp;limit={{.Limit}}</a>
  &nbsp;·&nbsp;
  Change limit: <a href="/channel?msg_id={{.StartMsgID}}&limit=50">limit=50</a>
  &nbsp;/&nbsp;
  <a href="/channel?msg_id={{.StartMsgID}}&limit=200">limit=200</a>
</div>

<div class="filters">
  <button class="filter-btn active" data-type="all" onclick="setFilter('all',this)">All</button>
  <button class="filter-btn" data-type="video"    onclick="setFilter('video',this)">🎬 Video</button>
  <button class="filter-btn" data-type="audio"    onclick="setFilter('audio',this)">🎵 Audio</button>
  <button class="filter-btn" data-type="pdf"      onclick="setFilter('pdf',this)">📄 PDF</button>
  <button class="filter-btn" data-type="photo"    onclick="setFilter('photo',this)">🖼️ Photo</button>
  <button class="filter-btn" data-type="document" onclick="setFilter('document',this)">📦 Document</button>
  <button class="filter-btn" data-type="none"     onclick="setFilter('none',this)">💬 Text</button>
</div>

<div class="grid" id="grid">
{{range .Items}}
<div class="card type-{{.MediaType}}" data-type="{{.MediaType}}"
     data-name="{{.FileName}}" data-caption="{{.Caption}}">
  <div class="card-stripe"></div>
  <div class="card-body">
    <div class="card-header-row">
      <span class="type-badge">
        {{if eq .MediaType "video"}}🎬 Video
        {{else if eq .MediaType "audio"}}🎵 Audio
        {{else if eq .MediaType "pdf"}}📄 PDF
        {{else if eq .MediaType "photo"}}🖼️ Photo
        {{else if eq .MediaType "document"}}📦 Document
        {{else}}💬 Text{{end}}
      </span>
      <span class="msg-id">#{{.ID}}</span>
    </div>
    {{if .FileName}}<div class="file-name">{{.FileName}}</div>{{end}}
    {{if .Caption}}<div class="caption">{{.Caption}}</div>{{end}}
    <div class="meta">
      <span>🕐 {{.Date}}</span>
      {{if .FileSize}}<span>💾 {{formatSize .FileSize}}</span>{{end}}
      {{if .MimeType}}<span>🏷️ {{.MimeType}}</span>{{end}}
      {{if .Views}}<span>👁️ {{.Views}}</span>{{end}}
    </div>
  </div>
  {{if .HasMedia}}
  <div class="card-actions">
    {{if eq .MediaType "video"}}<a href="{{.StreamURL}}" target="_blank" class="btn btn-watch">▶ Watch</a>
    {{else if eq .MediaType "audio"}}<a href="{{.StreamURL}}" target="_blank" class="btn btn-listen">🎵 Listen</a>
    {{else if eq .MediaType "pdf"}}<a href="{{.StreamURL}}" target="_blank" class="btn btn-view">📄 View PDF</a>
    {{else if eq .MediaType "photo"}}<a href="{{.StreamURL}}" target="_blank" class="btn btn-photo">🖼️ View Photo</a>
    {{else}}<a href="{{.StreamURL}}" target="_blank" class="btn btn-open">📂 Open</a>{{end}}
    <a href="{{.DownloadURL}}" class="btn btn-dl">⬇ Download</a>
  </div>
  {{end}}
</div>
{{else}}
<div class="empty">
  <span class="emoji">📭</span>
  <p>No messages found. Try <a href="/channel?msg_id=1&limit=100" style="color:var(--accent)">starting from msg_id=1</a>.</p>
</div>
{{end}}
</div>

<div class="pagination">
  {{if gt .StartMsgID 1}}
    <a href="/channel?msg_id={{.PrevMsgID}}&limit={{.Limit}}">← Prev {{.Limit}}</a>
    <a href="/channel?msg_id=1&limit={{.Limit}}">⏮ From Start</a>
  {{end}}
  {{if gt .NextMsgID 0}}
    <a href="/channel?msg_id={{.NextMsgID}}&limit={{.Limit}}">Next {{.Limit}} →</a>
  {{end}}
</div>

<script>
  let currentFilter='all';
  function setFilter(type,btn){
    currentFilter=type;
    document.querySelectorAll('.filter-btn').forEach(b=>b.classList.remove('active'));
    btn.classList.add('active');
    applyFilters();
  }
  function applyFilters(){
    const q=document.getElementById('searchInput').value.toLowerCase().trim();
    document.querySelectorAll('.card').forEach(card=>{
      const tm=currentFilter==='all'||card.dataset.type===currentFilter;
      const n=(card.dataset.name||'').toLowerCase();
      const c=(card.dataset.caption||'').toLowerCase();
      const sm=!q||n.includes(q)||c.includes(q);
      card.style.display=tm&&sm?'':'none';
    });
  }
</script>
</body>
</html>`
