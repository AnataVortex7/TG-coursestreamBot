// internal/wmqueue/wmqueue.go
// ── Watermark Job Queue ───────────────────────────────────────────────────────
// Separate package — commands aani routes dono import karto, cycle nahi.
//
// commands → wmqueue  ✅
// routes   → wmqueue  ✅
// wmqueue  → internal/db (colab poll tracking, replica-safe) + stdlib + gin  ✅
// (internal/db कुठलाही internal package import करत नाही — cycle नाही.)

package wmqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ── Job Status ────────────────────────────────────────────────────────────────

const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusDone       = "done"
	StatusFailed     = "failed"
)

// ── WatermarkJob ──────────────────────────────────────────────────────────────

type WatermarkJob struct {
	JobID            string `json:"job_id"`
	FileURL          string `json:"file_url"`
	FileName         string `json:"file_name"`
	FileSize         int64  `json:"file_size"`
	WatermarkText    string `json:"watermark_text"`
	BotToken         string `json:"bot_token"`
	ChatID           int64  `json:"chat_id"`
	MsgID            int    `json:"msg_id"`
	ProgressMsgID    int    `json:"progress_msg_id"`
	SendToChatID     int64  `json:"send_to_chat_id"`
	SendToAccessHash int64  `json:"send_to_access_hash,omitempty"`
	SendToTopicID    int    `json:"send_to_topic_id,omitempty"`
	LogChatID        int64  `json:"log_chat_id,omitempty"`
	LogAccessHash    int64  `json:"log_access_hash,omitempty"`
	UserbotLogChatID  int64 `json:"userbot_log_chat_id,omitempty"`
	UserbotLogAccessHash int64 `json:"userbot_log_access_hash,omitempty"`
	UserbotLogTopicID int   `json:"userbot_log_topic_id,omitempty"`
	// ProtectContent — true असेल तरच Colab worker पाठवलेल्या video वर
	// forward/save block (protect_content) लावतो. Offline course student
	// watermark delivery साठी true, /userbotdl admin uploads साठी false —
	// त्यामुळे same WatermarkJob / same worker code वापरूनही दोन्ही cases
	// वेगळं वागतात.
	ProtectContent bool   `json:"protect_content,omitempty"`
	// LogOnly — [NEW] true असेल तर worker फक्त LOG_CHANNEL ला video पाठवतो
	// (unprotected master), student ला काहीच पाठवत नाही. Admin च्या silent
	// pre-download साठी — user ला annoy न करता cache आधीच भरून ठेवायला.
	LogOnly        bool   `json:"log_only,omitempty"`
	Caption        string `json:"caption"`
	// ── Direct-download fields (course offline-token flow only) ────────────
	// हे दोन्ही set असतील तरच worker main server च्या pipe/file_url ऐवजी
	// स्वतः थेट Telegram वरून (own Pyrogram session, DownloadBotToken वापरून)
	// source channel मधून file fetch करतो. /userbotdl (admin) flow अजूनही
	// जुनाच file_url/pipe मार्ग वापरतो — तिथे हे दोन्ही रिकामे/0 राहतात.
	ChannelID         int64     `json:"channel_id,omitempty"`
	ChannelAccessHash int64     `json:"channel_access_hash,omitempty"`
	DownloadBotToken  string    `json:"download_bot_token,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	Status            string    `json:"status"`
	Error             string    `json:"error,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
	WorkerID          string    `json:"-"` // job ज्या Colab worker ला assign झाला आहे



	// SentMessageID — Colab ने प्रत्यक्ष पाठवलेल्या message चा ID (student
	// offline-download साठी 48hr auto-delete schedule करायला लागतो). Colab
	// script "/api/colab/done" call करताना हे पाठवतो (नसेल तर 0 राहतो —
	// जुना Colab script अजूनही काम करत राहील, फक्त auto-delete मिळणार नाही).
	SentMessageID int `json:"-"`

	// LogChannelMsgID — worker ने LOG_CHANNEL ला पाठवलेल्या (unprotected
	// master) copy चा message ID. "/api/colab/done" call मध्ये worker हे
	// पाठवतो (नसेल तर 0). हा मिळाल्यावर caller (deliverViaColab) db.
	// UpsertDeliveredCache मध्ये save करतो — त्याच user ने तीच lecture परत
	// मागितली तर नवीन watermark job न चालवता, इथून थेट forward होतो
	// (CPU/वेळ वाचतो).
	LogChannelMsgID int `json:"-"`

	// ── Progress ordering ──────────────────────────────────────────────────
	// LastProgressSeq — आता पर्यंत apply झालेला सर्वात मोठा sequence number.
	// कमी/equal seq असलेला update stale समजून skip केला जातो (out-of-order
	// network arrival मुळे जुना % नंतर येणं टाळण्यासाठी).
	LastProgressSeq int64 `json:"-"`

	// progressBusy — सध्या एक EditTgMessage call चालू आहे का (in-flight).
	// खरं असेल तर पुढचा progress update तो complete होईपर्यंत skip होतो —
	// त्यामुळे एका वेळी फक्त एकच edit Telegram कडे जातो, ordering राहतं.
	progressBusy bool `json:"-"`

	// lastEditAttempt — शेवटचा edit attempt कधी केला (यशस्वी असो वा नसो).
	// TryProgress यावरून minimum spacing (baseEditGap) लागू करतो — प्रत्येक
	// progress tick ला लगेच Telegram ला hit न करता, थोडा वेळ थांबतो.
	lastEditAttempt time.Time `json:"-"`

	// editFailStreak — सलग किती edit attempts (MTProto + HTTP दोन्ही) fail
	// झाले. वाढत गेल्यास exponential backoff लागू होतो (कॅप्ड) — त्यामुळे
	// FLOOD_WAIT सारख्या दीर्घ बंदी दरम्यान दर काही सेकंदाला निष्फळ TLS
	// connections उघडत राहणं थांबतं. एक edit यशस्वी झाला की 0 वर reset होतो.
	editFailStreak int `json:"-"`

	// LastProgressMsg — [NEW] worker ने पाठवलेला सर्वात अलीकडचा free-text
	// stage/percent update (उदा. "📥 Downloading 63%..."). हा नेहमी साठवला
	// जातो — job.ProgressMsgID==0 असला (उदा. silent /predownload) तरी सुद्धा,
	// जेणेकरून कुठलाही caller (Telegram edit न करताही) सध्याचा stage वाचू
	// शकेल — उदा. admin च्या predownload progress-bar साठी.
	LastProgressMsg string `json:"-"`
}

// ── Colab Worker Online Tracker (per-worker) ──────────────────────────────────
// Multiple Colab instances (same public notebook, same secret key) parallel
// live असू शकतात — प्रत्येकाचा स्वतःचा WORKER_ID (uuid, notebook restart वर
// नवीन) असतो. प्रत्येक poll मध्ये worker_id येतो — त्याचा lastPollTime track
// करतो. 10 sec (2 missed polls @ 5s) मध्ये poll आला नाही → तो worker offline.

const workerOfflineThreshold = 90 * time.Second // 18 missed polls @ 5s — heavy video processing or network lag without false offline

// [FIX] आधी हे फक्त एका process cha in-memory map मध्ये track व्हायचं —
// main-server एकापेक्षा जास्त replicas/containers वर चालत असेल (उदा. काही
// hosting platforms हे स्वतःहून करतात) तर एका replica ला poll मिळाला तरी
// दुसऱ्या replica ला (जो admin चा /colabstatus command handle करतोय) तो
// कधीच दिसायचा नाही — "worker online असूनही कायम offline दिसणं" हा नेमका
// bug यातूनच येत होता. आता खरं state DB मध्ये (सगळ्या replicas मध्ये
// shared) ठेवलं आहे — कुठलाही replica आता बरोबर स्थिती दाखवेल.
//
// स्थानिक map अजूनही ठेवला आहे — फक्त याच process च्या पटीत (जसं की एका
// request मध्ये लागोपाठ बरेच checks) DB ला दरवेळी hit न करता जलद वाचता यावं
// म्हणून — पण खरा source-of-truth आता DB आहे.

var localPollCache = struct {
	mu       sync.Mutex
	lastPoll map[string]time.Time
}{lastPoll: make(map[string]time.Time)}

// RecordPoll — workerID च्या poll चा timestamp नोंदवतो (DB + local cache दोन्हीकडे).
func RecordPoll(workerID string) {
	if workerID == "" {
		return
	}
	localPollCache.mu.Lock()
	localPollCache.lastPoll[workerID] = time.Now()
	localPollCache.mu.Unlock()

	// SKIP_DB_WORKERS=true → DB ला लिहायचं नाही.
	// नाहीतर old machine ला new machine चे workers दिसतील → conflict.
	if config.ValueOf.SkipDBWorkers {
		return
	}
	if err := db.RecordColabPoll(workerID); err != nil {
		log.Printf("wmqueue: RecordColabPoll (DB) failed for worker %s: %v", workerID, err)
	}
}

// IsColabOnline — कुठलाही 1 worker तरी online आहे का (overall system status).
// SKIP_DB_WORKERS=true → local cache वापरतो (फक्त या machine ला poll केलेले workers).
// नाहीतर DB मधून बघतो (सगळ्या replicas मध्ये accurate राहण्यासाठी).
func IsColabOnline() bool {
	if config.ValueOf.SkipDBWorkers {
		return isOnlineLocalFallback()
	}
	online, err := db.IsColabOnlineDB(workerOfflineThreshold)
	if err != nil {
		log.Printf("wmqueue: IsColabOnlineDB failed, falling back to local cache: %v", err)
		return isOnlineLocalFallback()
	}
	return online
}

func isOnlineLocalFallback() bool {
	localPollCache.mu.Lock()
	defer localPollCache.mu.Unlock()
	for _, t := range localPollCache.lastPoll {
		if time.Since(t) < workerOfflineThreshold {
			return true
		}
	}
	return false
}

// ActiveWorkers — सध्या online असलेले worker IDs.
// SKIP_DB_WORKERS=true → local cache (फक्त या machine ला poll केलेले).
func ActiveWorkers() []string {
	if config.ValueOf.SkipDBWorkers {
		localPollCache.mu.Lock()
		defer localPollCache.mu.Unlock()
		var ids []string
		for id, t := range localPollCache.lastPoll {
			if time.Since(t) < workerOfflineThreshold {
				ids = append(ids, id)
			}
		}
		return ids
	}
	ids, err := db.ActiveColabWorkersDB(workerOfflineThreshold)
	if err != nil {
		log.Printf("wmqueue: ActiveColabWorkersDB failed: %v", err)
		return nil
	}
	return ids
}

// LastPollAgo — सर्वात recent poll कधी झाला.
// SKIP_DB_WORKERS=true → local cache वापरतो.
func LastPollAgo() string {
	if config.ValueOf.SkipDBWorkers {
		localPollCache.mu.Lock()
		defer localPollCache.mu.Unlock()
		var latest time.Time
		for _, t := range localPollCache.lastPoll {
			if t.After(latest) {
				latest = t
			}
		}
		if latest.IsZero() {
			return "never"
		}
		return time.Since(latest).Round(time.Second).String()
	}
	latest, err := db.LastColabPollAgoDB()
	if err != nil {
		log.Printf("wmqueue: LastColabPollAgoDB failed: %v", err)
		return "unknown (DB error)"
	}
	if latest.IsZero() {
		return "never"
	}
	return time.Since(latest).Round(time.Second).String()
}

// snapshotOnlineWorkers — सध्या online असलेल्या workers चा set (StartColabWatcher साठी).
// SKIP_DB_WORKERS=true → local cache वापरतो.
func snapshotOnlineWorkers() map[string]bool {
	if config.ValueOf.SkipDBWorkers {
		localPollCache.mu.Lock()
		defer localPollCache.mu.Unlock()
		set := make(map[string]bool, len(localPollCache.lastPoll))
		for id, t := range localPollCache.lastPoll {
			if time.Since(t) < workerOfflineThreshold {
				set[id] = true
			}
		}
		return set
	}
	ids, err := db.ActiveColabWorkersDB(workerOfflineThreshold)
	if err != nil {
		log.Printf("wmqueue: snapshotOnlineWorkers (DB) failed: %v", err)
		return map[string]bool{}
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// forgetWorker — worker बराच वेळ offline राहिला तर tracker मधून काढतो.
func forgetWorker(workerID string) {
	localPollCache.mu.Lock()
	delete(localPollCache.lastPoll, workerID)
	localPollCache.mu.Unlock()

	if config.ValueOf.SkipDBWorkers {
		return
	}
	if err := db.ForgetColabWorkerDB(workerID); err != nil {
		log.Printf("wmqueue: ForgetColabWorkerDB failed for worker %s: %v", workerID, err)
	}
}

// ── Colab Stop Flag ────────────────────────────────────────────────────────────
// Admin command (/colabstop) ने set होतो — Colab worker पुढच्या poll वर
// `stop: true` बघून loop graceful exit करतो. बाकी bot/server normal चालतो,
// फक्त Colab polling loop थांबतो.

var colabStop = struct {
	mu      sync.Mutex
	stopped bool
}{}

func RequestColabStop() {
	colabStop.mu.Lock()
	defer colabStop.mu.Unlock()
	colabStop.stopped = true
}

func ClearColabStop() {
	colabStop.mu.Lock()
	defer colabStop.mu.Unlock()
	colabStop.stopped = false
}

func IsColabStopRequested() bool {
	colabStop.mu.Lock()
	defer colabStop.mu.Unlock()
	return colabStop.stopped
}

// ── Job Queue ─────────────────────────────────────────────────────────────────

type Queue struct {
	mu      sync.Mutex
	jobs    map[string]*WatermarkJob
	pending []string
}

var Default = &Queue{
	jobs:    make(map[string]*WatermarkJob),
	pending: []string{},
}

func saveJobToDB(job *WatermarkJob) {
	if db.ColWMJobs == nil || job == nil || job.JobID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := options.Update().SetUpsert(true)
	_, _ = db.ColWMJobs.UpdateOne(ctx,
		bson.M{"job_id": job.JobID},
		bson.M{"$set": job},
		opts,
	)
}

func deleteJobFromDB(jobID string) {
	if db.ColWMJobs == nil || jobID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = db.ColWMJobs.DeleteOne(ctx, bson.M{"job_id": jobID})
}

func loadUnfinishedJobsFromDB() {
	if db.ColWMJobs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cursor, err := db.ColWMJobs.Find(ctx, bson.M{
		"status": bson.M{"$in": []string{StatusPending, StatusProcessing}},
	})
	if err != nil {
		return
	}
	defer cursor.Close(ctx)

	var loadedJobs []*WatermarkJob
	if err := cursor.All(ctx, &loadedJobs); err == nil && len(loadedJobs) > 0 {
		Default.mu.Lock()
		for _, job := range loadedJobs {
			if _, exists := Default.jobs[job.JobID]; !exists {
				job.Status = StatusPending
				Default.jobs[job.JobID] = job
				Default.pending = append(Default.pending, job.JobID)
			}
		}
		Default.mu.Unlock()
		log.Printf("[wmqueue] Recovered %d unfinished jobs from MongoDB", len(loadedJobs))
	}
}

func (q *Queue) Enqueue(job *WatermarkJob) {
	q.mu.Lock()
	job.Status = StatusPending
	job.CreatedAt = time.Now()
	job.UpdatedAt = time.Now()
	q.jobs[job.JobID] = job
	q.pending = append(q.pending, job.JobID)
	q.mu.Unlock()

	saveJobToDB(job)
}

// DynamicMaxJobsPerWorker - Calculates maximum jobs per worker dynamically.
// Default base is 2. If there are many pending jobs and fewer workers,
// the cap is increased to scale parallel processing. Maximum cap is 5
// to prevent Out-Of-Memory or Disk-Full errors on Colab/HuggingFace workers.
func (q *Queue) DynamicMaxJobsPerWorker() int {
	onlineWorkers := len(ActiveWorkers())
	if onlineWorkers == 0 {
		onlineWorkers = 1
	}
	
	pendingCount := 0
	// We are already inside a lock usually when this is called, but let's just 
	// be careful. If this is called from Poll() which holds the lock, we shouldn't lock again.
	// Oh wait, ActiveWorkers() does DB query, which is fine, but checking q.pending needs lock.
	// Since Poll() calls this while holding q.mu, we must NOT lock here.
	pendingCount = len(q.pending)

	baseCap := 2
	
	if pendingCount > onlineWorkers*baseCap {
		neededCap := (pendingCount + onlineWorkers - 1) / onlineWorkers
		if neededCap > baseCap {
			baseCap = neededCap
		}
	}
	
	const hardMax = 5
	if baseCap > hardMax {
		baseCap = hardMax
	}
	
	return baseCap
}

func (q *Queue) Poll(workerID string) *WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()

	maxJobs := q.DynamicMaxJobsPerWorker()

	// worker कडे cap पेक्षा जास्त jobs process चालू आहेत का -
	// live/normal आणि bulk jobs सर्व count होतात.
	activeForWorker := 0
	if workerID != "" {
		for _, job := range q.jobs {
			if job.Status == StatusProcessing && job.WorkerID == workerID {
				activeForWorker++
			}
		}
	}

	var skipped []string // cap मुळे skip केलेले job IDs — शेवटी परत pending मध्ये टाकायला
	defer func() {
		if len(skipped) > 0 {
			q.pending = append(q.pending, skipped...)
		}
	}()

	for len(q.pending) > 0 {
		id := q.pending[0]
		q.pending = q.pending[1:]
		job, ok := q.jobs[id]
		if !ok || job.Status != StatusPending {
			continue
		}

		if workerID != "" && activeForWorker >= maxJobs {
			// हा worker आधीच cap इतके jobs घेऊन बसलाय — हा job
			// skip करून पुढचा job बघतो. cap मुळे
			// skip केलेला job हरवू नये म्हणून queue च्या शेवटी परत टाकतो.
			skipped = append(skipped, id)
			continue
		}

		job.Status = StatusProcessing
		job.WorkerID = workerID
		job.UpdatedAt = time.Now()
		return job
	}
	return nil
}

func (q *Queue) UpdateStatus(jobID, status, errMsg string) {
	q.mu.Lock()
	var targetJob *WatermarkJob
	if job, ok := q.jobs[jobID]; ok {
		job.Status = status
		job.Error = errMsg
		job.UpdatedAt = time.Now()
		targetJob = job
	}
	q.mu.Unlock()

	if status == StatusDone || status == StatusFailed {
		deleteJobFromDB(jobID)
	} else if targetJob != nil {
		saveJobToDB(targetJob)
	}
}

// SetSentMessageID — Colab ने प्रत्यक्ष पाठवलेल्या message चा ID साठवतो (student
// offline-download flow साठी 48hr auto-delete schedule करायला हा लागतो).
func (q *Queue) SetSentMessageID(jobID string, msgID int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if job, ok := q.jobs[jobID]; ok {
		job.SentMessageID = msgID
	}
}

// SetLogChannelMsgID — worker ने LOG_CHANNEL ला पाठवलेल्या master copy चा
// message ID साठवतो (DeliveredCache fast-path साठी).
func (q *Queue) SetLogChannelMsgID(jobID string, msgID int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if job, ok := q.jobs[jobID]; ok {
		job.LogChannelMsgID = msgID
	}
}

// SetProgressMessage — [NEW] worker कडून आलेला latest stage/percent text
// साठवतो (Telegram edit होवो अथवा न होवो — ProgressMsgID शी संबंधित नाही).
func (q *Queue) SetProgressMessage(jobID, msg string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if job, ok := q.jobs[jobID]; ok {
		job.LastProgressMsg = msg
		job.UpdatedAt = time.Now()
	}
}

// SetProgressMessage — [NEW] package-level wrapper (Default queue साठी), बाकी
// package-level helpers (ActiveWorkers, IsColabOnline, वगैरे) सारखाच पॅटर्न.
func SetProgressMessage(jobID, msg string) {
	Default.SetProgressMessage(jobID, msg)
}

// ProgressMessage — [NEW] एखाद्या jobID चा सध्याचा latest stage/percent text
// वाचतो (उदा. admin च्या predownload progress-bar साठी per-job detail).
func (q *Queue) ProgressMessage(jobID string) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[jobID]
	if !ok || job.LastProgressMsg == "" {
		return "", false
	}
	return job.LastProgressMsg, true
}

// ProgressMessage — [NEW] package-level wrapper (Default queue साठी), बाकी
// package-level helpers (ActiveWorkers, IsColabOnline, वगैरे) सारखाच पॅटर्न.
func ProgressMessage(jobID string) (string, bool) {
	return Default.ProgressMessage(jobID)
}

// ── TryProgress ───────────────────────────────────────────────────────────────
// एका वेळी फक्त एक progress edit Telegram कडे जावा यासाठी ordering+lock:
//   - seq <= LastProgressSeq → stale (out-of-order/late) update, skip करा.
//   - मागचा edit अजून चालू आहे (progressBusy) → हा update skip करा, पुढचा
//     update येईल तेव्हा त्या वेळची latest progress दाखवली जाईल.
//
// दोन्ही cases मध्ये ok=false. ok=true असेल तर caller ने edit सुरू केल्यावर
// FinishProgress() call करून busy flag clear करावा.
// baseEditGap — दोन Telegram edit attempts मध्ये minimum अंतर (जरी progress
// खूप वेगात येत असला तरी). maxEditBackoff — सलग failures वर backoff याच्या
// पुढे कधीच जात नाही (job कायमचा "silently stuck" वाटू नये म्हणून periodic
// retry चालूच राहतो, फक्त हळू).
const (
	baseEditGap    = 3 * time.Second
	maxEditBackoff = 2 * time.Minute
)

func (q *Queue) TryProgress(jobID string, seq int64) (job *WatermarkJob, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, found := q.jobs[jobID]
	if !found {
		return nil, false
	}
	if seq <= j.LastProgressSeq {
		return nil, false // stale, out-of-order
	}
	if j.progressBusy {
		return nil, false // mागcha edit chalu ahe, ha update skip
	}
	// [FIX] Minimum spacing — आधी फक्त concurrency check होता, त्यामुळे edit
	// अयशस्वी होऊन लगेच परत मोकळा (progressBusy=false) झाला की पुढचा update
	// लगेच पुन्हा प्रयत्न करायचा — जलद progress ticks (उदा. दर 1 sec) वर हे
	// प्रत्यक्षात दर काही sec ला Telegram ला hit करत राहायचं (FLOOD_WAIT
	// दरम्यानही निष्फळ HTTP fallback attempts सुरूच राहायचे). आता:
	// सलग failures वाढले की gap वाढतो (capped), यशस्वी edit वर परत base gap.
	gap := baseEditGap
	if j.editFailStreak > 0 {
		backoff := time.Duration(j.editFailStreak) * baseEditGap * 2
		if backoff > maxEditBackoff {
			backoff = maxEditBackoff
		}
		gap = backoff
	}
	if !j.lastEditAttempt.IsZero() && time.Since(j.lastEditAttempt) < gap {
		return nil, false // too soon — पुढच्या tick ला परत प्रयत्न होईल
	}
	j.LastProgressSeq = seq
	j.progressBusy = true
	j.lastEditAttempt = time.Now()
	return j, true
}

// FinishProgress — TryProgress नंतर edit complete/fail झाल्यावर busy flag clear
// करतो, taki पुढचा progress update pick होऊ शकेल. success सांगतो actual edit
// (MTProto किंवा HTTP, कुठलाही मार्ग) यशस्वी झाला का — त्यावरून editFailStreak
// reset/increment होतो, जो पुढच्या backoff gap साठी वापरला जातो.
func (q *Queue) FinishProgress(jobID string, success bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if j, ok := q.jobs[jobID]; ok {
		j.progressBusy = false
		if success {
			j.editFailStreak = 0
		} else {
			j.editFailStreak++
		}
	}
}

func (q *Queue) Get(jobID string) (*WatermarkJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[jobID]
	return job, ok
}

// (OutputPath / ChunkDir helpers काढले — Colab आता थेट Telethon ने
//  Telegram la send करतो, Koyeb ला processed file परत येत नाही.)

func (q *Queue) Cleanup() {
	q.mu.Lock()
	defer q.mu.Unlock()
	cutoff := time.Now().Add(-2 * time.Hour)
	for id, job := range q.jobs {
		if job.UpdatedAt.Before(cutoff) {
			delete(q.jobs, id)
			continue
		}
		// Time-limit नाही — Colab offline असेल तर StartColabWatcher
		// processing jobs ला परत pending करतो (online झाल्यावर fresh resume).
	}
}

// ── RequeueStaleProcessingForWorker ───────────────────────────────────────────
// एक specific Colab worker offline झाला — फक्त त्याच worker कडे assign झालेले
// "processing" jobs परत "pending" मध्ये टाकतो (इतर online workers चे जॉब्स
// untouched राहतात). Online झाल्यावर तेच job परत Poll() मधून मिळतील आणि
// त्याच ProgressMsgID पासून fresh सुरू होतील.

func (q *Queue) RequeueStaleProcessingForWorker(workerID string) []*WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var affected []*WatermarkJob
	for _, job := range q.jobs {
		if job.Status == StatusProcessing && job.WorkerID == workerID && time.Since(job.UpdatedAt) > 3*time.Minute {
			job.Status = StatusPending
			job.WorkerID = ""
			job.LastProgressSeq = 0
			job.progressBusy = false
			job.UpdatedAt = time.Now()
			q.pending = append([]string{job.JobID}, q.pending...)
			affected = append(affected, job)
		}
	}
	return affected
}

// ── RequeueOrphanedProcessing ─────────────────────────────────────────────────
// नवीन (किंवा परत) Colab worker online झाला तेव्हा call होतो — फक्त त्याच नवीन
// worker च्या जुन्या WorkerID शी match शोधण्यावर अवलंबून न राहता, सध्या
// "processing" status मधले आणि सध्या कुठल्याही online worker ला assign
// नसलेले (म्हणजे त्यांचा WorkerID onlineSet मध्ये नाही) असे सगळे jobs परत
// "pending" मध्ये टाकतो. हे navin worker ID generate होत असल्यामुळे
// (notebook restart वर uuid बदलतो) old↔new WorkerID mismatch मुळे job
// कायमचा orphaned राहण्याची समस्या टाळतो.
func (q *Queue) RequeueOrphanedProcessing(onlineSet map[string]bool) []*WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var affected []*WatermarkJob
	for _, job := range q.jobs {
		if job.Status == StatusProcessing && !onlineSet[job.WorkerID] && time.Since(job.UpdatedAt) > 3*time.Minute {
			job.Status = StatusPending
			job.WorkerID = ""
			job.LastProgressSeq = 0
			job.progressBusy = false
			job.UpdatedAt = time.Now()
			q.pending = append([]string{job.JobID}, q.pending...)
			affected = append(affected, job)
		}
	}
	return affected
}

// ── ProcessingJobs ───────────────────────────────────────────────────────────
// सध्या "processing" status मधले सगळे jobs (notification साठी)

func (q *Queue) ProcessingJobs() []*WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var list []*WatermarkJob
	for _, job := range q.jobs {
		if job.Status == StatusProcessing {
			list = append(list, job)
		}
	}
	return list
}

// ── ProcessingJobsForWorker ────────────────────────────────────────────────────
// specific workerID कडे assign असलेले "processing" jobs

func (q *Queue) ProcessingJobsForWorker(workerID string) []*WatermarkJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var list []*WatermarkJob
	for _, job := range q.jobs {
		if job.Status == StatusProcessing && job.WorkerID == workerID {
			list = append(list, job)
		}
	}
	return list
}

// ── Temp File Registry ────────────────────────────────────────────────────────

type tempFileEntry struct {
	FilePath     string
	FileName     string
	RegisteredAt time.Time
}

var tfRegistry = struct {
	mu    sync.Mutex
	files map[string]*tempFileEntry
}{
	files: make(map[string]*tempFileEntry),
}

func RegisterTempFile(token, filePath, fileName string) {
	tfRegistry.mu.Lock()
	defer tfRegistry.mu.Unlock()
	tfRegistry.files[token] = &tempFileEntry{
		FilePath:     filePath,
		FileName:     fileName,
		RegisteredAt: time.Now(),
	}
}

func UnregisterTempFile(token string) {
	tfRegistry.mu.Lock()
	defer tfRegistry.mu.Unlock()
	delete(tfRegistry.files, token)
}

func GetTempFile(token string) (filePath, fileName string, ok bool) {
	tfRegistry.mu.Lock()
	defer tfRegistry.mu.Unlock()
	entry, found := tfRegistry.files[token]
	if !found {
		return "", "", false
	}
	return entry.FilePath, entry.FileName, true
}

func CleanupTempFiles() {
	tfRegistry.mu.Lock()
	defer tfRegistry.mu.Unlock()
	cutoff := time.Now().Add(-4 * time.Hour)
	for token, entry := range tfRegistry.files {
		if entry.RegisteredAt.Before(cutoff) {
			delete(tfRegistry.files, token)
		}
	}
}

// ── Pipe Registry ─────────────────────────────────────────────────────────────

type PipeFn func(w http.ResponseWriter) error

type pipeEntry struct {
	Fn           PipeFn
	FileName     string
	FileSize     int64
	RegisteredAt time.Time
}

var pipeRegistry = struct {
	mu    sync.Mutex
	pipes map[string]*pipeEntry
}{
	pipes: make(map[string]*pipeEntry),
}

func RegisterPipe(token, fileName string, fileSize int64, fn PipeFn) {
	pipeRegistry.mu.Lock()
	defer pipeRegistry.mu.Unlock()
	pipeRegistry.pipes[token] = &pipeEntry{
		Fn:           fn,
		FileName:     fileName,
		FileSize:     fileSize,
		RegisteredAt: time.Now(),
	}
}

func UnregisterPipe(token string) {
	pipeRegistry.mu.Lock()
	defer pipeRegistry.mu.Unlock()
	delete(pipeRegistry.pipes, token)
}

func GetPipe(token string) (fileName string, fileSize int64, fn PipeFn, ok bool) {
	pipeRegistry.mu.Lock()
	defer pipeRegistry.mu.Unlock()
	entry, found := pipeRegistry.pipes[token]
	if !found {
		return "", 0, nil, false
	}
	return entry.FileName, entry.FileSize, entry.Fn, true
}

func CleanupPipes() {
	pipeRegistry.mu.Lock()
	defer pipeRegistry.mu.Unlock()
	cutoff := time.Now().Add(-4 * time.Hour)
	for token, entry := range pipeRegistry.pipes {
		if entry.RegisteredAt.Before(cutoff) {
			delete(pipeRegistry.pipes, token)
		}
	}
}

// ── SubmitJob ─────────────────────────────────────────────────────────────────

func SubmitJob(
	fileURL, fileName string,
	fileSize int64,
	wmText, botToken string,
	adminChatID int64,
	progressMsgID int,
	sendToChatID int64,
	sendToAccessHash int64,
	sendToTopicID int,
	logChatID int64,
	logAccessHash int64,
	userbotLogChatID int64,
	userbotLogAccessHash int64,
	userbotLogTopicID int,
	caption string,
	srcMsgID int,
	protectContent bool,
	logOnly bool, // [NEW] true = फक्त LOG_CHANNEL ला पाठव, student ला काहीच नाही
	channelID int64, // 0 असेल तर pipe/file_url mode (जुना, /userbotdl साठी)
	channelAccessHash int64, // worker ला Pyrogram peer-cache seed करण्यासाठी लागतो
	downloadBotToken string, // "" असेल तर pipe/file_url mode
) (string, error) {
	jobID := fmt.Sprintf("wm-%d-%d", time.Now().UnixNano(), rand.Intn(9999))
	job := &WatermarkJob{
		JobID:             jobID,
		FileURL:           fileURL,
		FileName:          fileName,
		FileSize:          fileSize,
		WatermarkText:     wmText,
		BotToken:          botToken,
		ChatID:            adminChatID,
		MsgID:             srcMsgID,
		ProgressMsgID:     progressMsgID,
		SendToChatID:      sendToChatID,
		SendToAccessHash:  sendToAccessHash,
		SendToTopicID:     sendToTopicID,
		LogChatID:         logChatID,
		LogAccessHash:     logAccessHash,
		UserbotLogChatID:  userbotLogChatID,
		UserbotLogAccessHash: userbotLogAccessHash,
		UserbotLogTopicID: userbotLogTopicID,
		Caption:           caption,
		ProtectContent:    protectContent,
		LogOnly:           logOnly,
		ChannelID:         channelID,
		ChannelAccessHash: channelAccessHash,
		DownloadBotToken:  downloadBotToken,
	}
	Default.Enqueue(job)
	return jobID, nil
}

// ── WaitForDone ───────────────────────────────────────────────────────────────
// Colab job poll karto, FFmpeg karto, aani स्वतः Telethon ने Telegram la
// processed file pathvतो. Koyeb फक्त job चा final status (done/failed) wait
// karto — output file Koyeb kade kadhich yet nahi.

func WaitForDone(ctx context.Context, jobID string) (success bool, errMsg string, sentMessageID int, logChannelMsgID int) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, "context cancelled", 0, 0
		case <-ticker.C:
			job, ok := Default.Get(jobID)
			if !ok {
				return false, "job not found in queue", 0, 0
			}
			switch job.Status {
			case StatusDone:
				return true, "", job.SentMessageID, job.LogChannelMsgID
			case StatusFailed:
				return false, job.Error, 0, 0
			}
		}
	}
}

// ── MTProtoEdit ───────────────────────────────────────────────────────────────
// run.go मधून wire होतो (import cycle टाळण्यासाठी DI — wmqueue फक्त stdlib+gin
// import करतो, gotgproto/tg नाही). Set असेल तर EditTgMessage आधी हाच वापरतो —
// bot च्या आधीच चालू असलेल्या persistent MTProto connection वरून थेट
// messages.editMessage पाठवतो, त्यामुळे प्रत्येक edit साठी नवीन HTTPS
// connection (api.telegram.org) उघडावी लागत नाही. काही होस्टिंग (उदा.
// Hugging Face Spaces) वरून त्या नवीन HTTPS connections ला वेळोवेळी वेळ जास्त
// लागतो/timeout (context deadline exceeded) होतो — तिथे progress edits
// पहिल्याच text वर freeze होतात. MTProtoEdit ते बायपास करतो.
//
// bool return: true = edit यशस्वी (आणखी काही करायची गरज नाही), false = हा
// path वापरता आला नाही (unset/client सापडला नाही/error) — EditTgMessage मग
// जुन्या HTTP Bot API मार्गावर fallback होतो.
var MTProtoEdit func(botToken string, chatID int64, msgID int, text string) bool

// MTProtoNotify — MTProto (gotgproto) connection चा वापर करून admin ला notification पाठवतो
// Hugging Face / shared hosting कडून HTTP api.telegram.org block/timeout (context deadline exceeded) बायपास करण्यासाठी.
var MTProtoNotify func(text string) bool

// ── EditTgMessage ─────────────────────────────────────────────────────────────
//
// आधी parse_mode:"Markdown" कायम वापरत होतो आणि response कधीच check करत
// नव्हतो — त्यामुळे कुठलाही error (उदा. filename मध्ये `_`/`*`/backtick असेल
// तर Telegram चा "can't parse entities" 400 error) पूर्णपणे silently गायब
// व्हायचा. परिणाम: पहिला (plain-text) message दिसायचा, पण त्यानंतरचे सगळे
// progress % edits (Markdown सकट) fail व्हायचे — म्हणून UI कायम पहिल्याच
// text वर freeze व्हायचं (नुसतं "Uploading..." दिसत राहायचं).
//
// आता: (0) आधी MTProtoEdit (जर wire केला असेल) — persistent MTProto
// connection वापरून, HTTP पेक्षा जास्त reliable (विशेषतः Hugging Face सारख्या
// hosting वर जिथे नवीन outbound HTTPS connections ला वेळ जास्त लागू शकतो).
// (1) Markdown सकट HTTP Bot API प्रयत्न करतो, (2) Telegram ने parse-error
// दिला तर plain text (parse_mode शिवाय) retry करतो — म्हणजे formatting शक्य
// असेल तिथे मिळतं, नसेल तिथे edit तरी होतोच. (3) failure असेल तर log होतो.
// ── Edit Flood-Wait Guard ──────────────────────────────────────────────────
// Telegram चा FLOOD_WAIT (rpc 420) एका बॉट session ला ठराविक काळासाठी (कधी
// कधी तासांसाठी) messages.editMessage करण्यापासून पूर्ण रोखतो. आधी असं होत
// होतं की FLOOD_WAIT आल्यावरही EditTgMessage दर काही सेकंदांनी परत MTProto +
// नंतर HTTP दोन्ही मार्गांनी प्रयत्न करत राहायचं — दोन्ही निष्फळ, आणि HTTP
// कडे तर प्रत्येक वेळी नवीन TLS connection उघडायचा प्रयत्न (costly + slow).
// इथे botToken नुसार "किती वेळ थांबायचं" साठवतो — run.go मधला MTProtoEdit
// wiring FLOOD_WAIT सेकंद parse करून SetEditFloodWait ला कळवतो. जोपर्यंत ती
// वेळ संपत नाही, तोपर्यंत EditTgMessage दोन्ही मार्ग (MTProto आणि HTTP)
// पूर्णपणे skip करतो — नुसता false return करतो, network ला हातही लावत नाही.
var editFloodWait = struct {
	mu    sync.Mutex
	until map[string]time.Time
}{until: make(map[string]time.Time)}

// SetEditFloodWait — botToken साठी FLOOD_WAIT expiry नोंदवतो. run.go च्या
// MTProtoEdit closure मधून, FLOOD_WAIT error parse केल्यावर call होतो.
func SetEditFloodWait(botToken string, until time.Time) {
	if botToken == "" {
		return
	}
	editFloodWait.mu.Lock()
	defer editFloodWait.mu.Unlock()
	// जुनी expiry नवीनपेक्षा नंतरची असेल तर तीच ठेवा (overwrite करून वेळ
	// कमी करू नये — अनेक goroutines एकाच वेळी वेगवेगळे FLOOD_WAIT parse
	// करत असतील तर सगळ्यात मोठा wait जिंकतो).
	if existing, ok := editFloodWait.until[botToken]; ok && existing.After(until) {
		return
	}
	editFloodWait.until[botToken] = until
}

// InEditFloodWait — सध्या या botToken साठी FLOOD_WAIT active आहे का.
func InEditFloodWait(botToken string) (time.Time, bool) {
	editFloodWait.mu.Lock()
	defer editFloodWait.mu.Unlock()
	until, ok := editFloodWait.until[botToken]
	if !ok {
		return time.Time{}, false
	}
	if time.Now().After(until) {
		delete(editFloodWait.until, botToken) // expired — साफ करा
		return time.Time{}, false
	}
	return until, true
}

func ClearEditFloodWait(botToken string) {
	editFloodWait.mu.Lock()
	defer editFloodWait.mu.Unlock()
	delete(editFloodWait.until, botToken)
}

// EditTgMessage — return value सांगतो edit प्रत्यक्षात यशस्वी झाला का (कुठल्याही
// मार्गाने — MTProto, Markdown HTTP, किंवा plain-text HTTP fallback). Caller
// (उदा. progress-update handler) हे FinishProgress(jobID, success) ला पास
// करून editFailStreak/backoff चालवतो — त्यामुळे सलग failures (FLOOD_WAIT सकट)
// आपोआप कमी वारंवार retry होतात, प्रत्येक tick ला निष्फळ प्रयत्न होत नाही.
func EditTgMessage(botToken string, chatID int64, msgID int, text string) bool {
	if until, waiting := InEditFloodWait(botToken); waiting {
		log.Printf("[wmqueue] EditTgMessage: skipping (FLOOD_WAIT active until %s, botToken=...%s)",
			until.Format(time.RFC3339), lastN(botToken, 6))
		return false
	}
	if MTProtoEdit != nil && MTProtoEdit(botToken, chatID, msgID, text) {
		return true
	}
	// [FIX] MTProtoEdit ने आत्ताच FLOOD_WAIT नोंदवला असेल (वरचा if fail झाला
	// त्यामुळे इथे पोहोचलो) तर लगेच परत तपासा — नाहीतर हाच call पुढे HTTP
	// (Markdown + plain, प्रत्येकी ~15 sec TLS timeout) दोन्ही निष्फळ प्रयत्न
	// करत बसतो, जरी आपल्याला आत्ताच कळलं की पुढचे 15+ तास काहीही उपयोग नाही.
	if _, waiting := InEditFloodWait(botToken); waiting {
		return false
	}
	if editTgMessageAttempt(botToken, chatID, msgID, text, true) {
		return true
	}
	// Markdown parse fail झाला असेल — plain text ने retry (हमखास यशस्वी होईल,
	// फक्त *bold*/`code` literal दिसेल, edit तरी होईल).
	return editTgMessageAttempt(botToken, chatID, msgID, text, false)
}

// lastN — log मध्ये पूर्ण bot token न दाखवता फक्त शेवटची काही अक्षरं (debugging
// साठी पुरेसं, token leak होण्याचा धोका नाही).
func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func editTgMessageAttempt(botToken string, chatID int64, msgID int, text string, useMarkdown bool) bool {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	body := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
		"text":       text,
	}
	if useMarkdown {
		body["parse_mode"] = "Markdown"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		log.Printf("[wmqueue] EditTgMessage: marshal error: %v", err)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		log.Printf("[wmqueue] EditTgMessage: request build error: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	client := wmHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[wmqueue] EditTgMessage: request failed (chatID=%d, msgID=%d): %v", chatID, msgID, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		// "message is not modified" ही निरुपद्रवी आहे (तेच text पुन्हा पाठवलं
		// गेलं) — तिला वेगळी treat करतो, बाकी सगळं खरं failure म्हणून log करतो.
		if bytes.Contains(respBody, []byte("message is not modified")) {
			return true
		}

		if resp.StatusCode == 429 {
			var result struct {
				Parameters struct {
					RetryAfter int `json:"retry_after"`
				} `json:"parameters"`
			}
			if err := json.Unmarshal(respBody, &result); err == nil && result.Parameters.RetryAfter > 0 {
				log.Printf("[wmqueue] EditTgMessage: HTTP 429 FLOOD_WAIT, retry_after=%d", result.Parameters.RetryAfter)
				SetEditFloodWait(botToken, time.Now().Add(time.Duration(result.Parameters.RetryAfter+2)*time.Second))
			}
		}

		log.Printf("[wmqueue] EditTgMessage: non-200 (markdown=%v, chatID=%d, msgID=%d): %d — %s",
			useMarkdown, chatID, msgID, resp.StatusCode, string(respBody))
		return false
	}
	return true
}

// ── SecretOK ──────────────────────────────────────────────────────────────────

func SecretOK(headerVal string) bool {
	secret := os.Getenv("COLAB_SECRET_KEY")
	if secret == "" {
		return false
	}
	return headerVal == secret
}

// ── StartCleanupLoop ──────────────────────────────────────────────────────────

func StartCleanupLoop() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			Default.Cleanup()
			CleanupTempFiles()
			CleanupPipes()
		}
	}()
}

// ── notifyAllowedUsers ─────────────────────────────────────────────────────────
// ALLOWED_USERS (config) मधल्या प्रत्येक user ला message पाठवतो — Colab
// online/offline notifications साठी. Message 30 sec नंतर auto-delete होतो.

func notifyAllowedUsers(text string) {
	if MTProtoNotify != nil && MTProtoNotify(text) {
		return
	}
	botToken := config.ValueOf.BotToken
	for _, uid := range config.ValueOf.AllowedUsers {
		go func(chatID int64) {
			msgID := SendTgMessage(botToken, chatID, text)
			if msgID == 0 {
				return
			}
			time.Sleep(30 * time.Second)
			DeleteTgMessage(botToken, chatID, msgID)
		}(uid)
	}
}

func SendTgMessage(botToken string, chatID int64, text string) int64 {
	if botToken == "" || chatID == 0 || strings.TrimSpace(text) == "" {
		log.Printf("[wmqueue] sendTgMessage: empty botToken, chatID, or text provided (skip)")
		return 0
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
	})
	if err != nil {
		log.Printf("[wmqueue] sendTgMessage: marshal error: %v", err)
		return 0
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
		if err != nil {
			cancel()
			log.Printf("[wmqueue] sendTgMessage: request build error: %v", err)
			return 0
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := wmHTTPClient.Do(req)
		if err != nil {
			cancel()
			if attempt < 3 {
				time.Sleep(time.Duration(attempt*2) * time.Second)
				continue
			}
			log.Printf("[wmqueue] sendTgMessage: http error (after 3 attempts): %v", err)
			return 0
		}
		
		var result struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
			Result      struct {
				MessageID int64 `json:"message_id"`
			} `json:"result"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		cancel()

		if err != nil {
			log.Printf("[wmqueue] sendTgMessage: decode error: %v", err)
			return 0
		}
		if !result.OK {
			log.Printf("[wmqueue] sendTgMessage: API error chatID=%d desc=%s — retrying without markdown", chatID, result.Description)
			return sendTgMessagePlain(botToken, chatID, text)
		}
		return result.Result.MessageID
	}
	return 0
}

// sendTgMessagePlain — Markdown parse fail झाल्यास plain text ने retry (escape issue मुळे
// message कधीच न पाठवलं जाण्याचा bug टाळण्यासाठी — त्यामुळे delete सुद्धा कधी होणार नाही).
func sendTgMessagePlain(botToken string, chatID int64, text string) int64 {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	})
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	client := wmHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[wmqueue] sendTgMessagePlain: http error: %v", err)
		return 0
	}
	defer resp.Body.Close()
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.OK {
		log.Printf("[wmqueue] sendTgMessagePlain: API error chatID=%d desc=%s", chatID, result.Description)
		return 0
	}
	return result.Result.MessageID
}

func DeleteTgMessage(botToken string, chatID int64, msgID int64) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", botToken)
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
	})
	if err != nil {
		log.Printf("[wmqueue] deleteTgMessage: marshal error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		log.Printf("[wmqueue] deleteTgMessage: request build error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := wmHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[wmqueue] deleteTgMessage: http error: %v", err)
		return
	}
	defer resp.Body.Close()
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && !result.OK {
		log.Printf("[wmqueue] deleteTgMessage: API error chatID=%d msgID=%d desc=%s", chatID, msgID, result.Description)
	}
}

// ── StartColabWatcher ─────────────────────────────────────────────────────────
// प्रत्येक Colab worker (workerID) चं online/offline state independently track
// करतो (multiple Colab instances parallel live असू शकतात — public notebook +
// shared secret key मुळे):
//   - worker online → offline: त्या worker कडे assign असलेल्या processing jobs
//     चे progress messages "⏸️ Colab worker offline" असे edit करतो + ALLOWED_USERS
//     ला notify करतो.
//   - worker offline → online (किंवा नवीन worker पहिल्यांदा दिसला): त्या worker कडे
//     अडकलेले processing jobs (RequeueStaleProcessingForWorker) परत pending मध्ये
//     टाकतो, त्याच ProgressMsgID वर "🔄 fresh restart" edit करतो, आणि
//     ALLOWED_USERS ला notify करतो.
//
// हे bot च्या इतर कुठल्याही functionality ला disturb करत नाही — फक्त wmqueue
// state वर काम करणारा independent goroutine आहे.

func StartColabWatcher() {
	loadUnfinishedJobsFromDB()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		previouslyOnline := make(map[string]bool)
		for range ticker.C {
			nowOnline := snapshotOnlineWorkers()

			// ── नवीन online झालेले / पहिल्यांदा दिसलेले workers ──────────────
			for id := range nowOnline {
				if !previouslyOnline[id] {
					affected := Default.RequeueOrphanedProcessing(nowOnline)
					for _, job := range affected {
						if job.ProgressMsgID > 0 {
							go EditTgMessage(job.BotToken, job.ChatID, job.ProgressMsgID,
								fmt.Sprintf("🔄 *Colab worker online झालं*\n📄 `%s`\n♻️ Job पुन्हा सुरू होतोय...", job.FileName))
						}
					}
				}
			}

			// ── आता offline झालेले workers ───────────────────────────────────
			for id := range previouslyOnline {
				if !nowOnline[id] {
					requeuedJobs := Default.RequeueStaleProcessingForWorker(id)
					for _, job := range requeuedJobs {
						if job.ProgressMsgID > 0 {
							onlineWorkers := ActiveWorkers()
							var statusMsg string
							if len(onlineWorkers) > 0 {
								statusMsg = fmt.Sprintf("⏸️ *Colab worker offline झालं*\n📄 `%s`\n🔄 दुसरा online worker आहे — job लवकरच resume होईल.", job.FileName)
							} else {
								statusMsg = fmt.Sprintf("⏸️ *Colab worker offline झालं*\n📄 `%s`\n🔄 कुठलाही worker online नाही — नवीन worker online झाल्यावर job resume होईल.", job.FileName)
							}
							go EditTgMessage(job.BotToken, job.ChatID, job.ProgressMsgID, statusMsg)
						}
					}
				}
			}

			previouslyOnline = nowOnline
		}
	}()
}

// shortID — लांब uuid worker ID पैकी फक्त सुरुवातीचे काही अक्षर दाखवण्यासाठी
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

var wmHTTPClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 30 * time.Second,
	},
	Timeout: 35 * time.Second,
}
