// internal/db/mongo.go
package db

import (
	"context"
	"log"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Global DB handle ────────────────────────────────────────────────────────

var (
	Client   *mongo.Client
	Database *mongo.Database

	ColUsers        *mongo.Collection
	ColCourses      *mongo.Collection
	ColAccess       *mongo.Collection
	ColSessions     *mongo.Collection
	ColChannelCache *mongo.Collection
	ColStreamNonces *mongo.Collection // per-page-load stream nonces (anti-piracy)
	ColWorkers      *mongo.Collection // external streaming workers
	ColOTPs         *mongo.Collection // email OTP login records (TTL: 10 min)
	ColPendingRegs  *mongo.Collection // unverified registration records (TTL: 10 min)
	ColURLMonitors   *mongo.Collection // url monitor — persisted monitored URLs
	ColWatchHistory  *mongo.Collection // user watch history + view counts
	ColAnnouncements *mongo.Collection // admin broadcast announcements
	ColFolders       *mongo.Collection // course folders (subject-wise grouping)
	ColSettings      *mongo.Collection // key-value app settings (toggles etc.)
)

// ─── Models ──────────────────────────────────────────────────────────────────

// User — website login user (self-registers or admin creates via bot)
type User struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"       json:"id"`
	Username      string             `bson:"username"            json:"username"`      // unique, auto-set to mobile or email if blank
	FullName      string             `bson:"full_name"           json:"full_name"`
	Mobile        string             `bson:"mobile"              json:"mobile"`        // e.g. "9876543210" (digits only, optional)
	Email         string             `bson:"email"               json:"email"`         // optional
	PasswordHash  string             `bson:"password_hash"       json:"-"`
	TelegramID    int64              `bson:"telegram_id"         json:"telegram_id"`   // linked Telegram user ID (0 = not linked)
	CreatedAt     time.Time          `bson:"created_at"          json:"created_at"`
	IsActive      bool               `bson:"is_active"           json:"is_active"`
	IsSpecialUser bool               `bson:"is_special_user"     json:"is_special_user"` // admin ne ON kelyavar - not-buyed courses cha content list disto (play nahi, fakt baghane)
}

// Session — one active session per user (new login kills old)
type Session struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	Token     string             `bson:"token"`       // JWT token string (for fast lookup)
	IP        string             `bson:"ip"`          // last known IP
	UserAgent string             `bson:"user_agent"`
	CreatedAt time.Time          `bson:"created_at"`
	ExpiresAt time.Time          `bson:"expires_at"`
}

// Course — a course admin creates via bot
type Course struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"  json:"id"`
	Title       string             `bson:"title"          json:"title"`
	Description string             `bson:"description"    json:"description"`
	Thumbnail   string             `bson:"thumbnail"      json:"thumbnail"`    // telegram file_id or URL
	Price       string             `bson:"price"          json:"price"`        // display string e.g. "₹999"
	BuyLink     string             `bson:"buy_link"       json:"buy_link"`     // payment / WhatsApp link
	ChannelID     int64            `bson:"channel_id"     json:"channel_id"`   // dedicated Telegram channel for this course
	IsActive      bool             `bson:"is_active"      json:"is_active"`
	CreatedAt     time.Time        `bson:"created_at"     json:"created_at"`
	DownloadLimit int              `bson:"download_limit" json:"download_limit"` // per-user daily offline download limit (0 = default 3)
}

// Access — which user can access which course (no expiry by design)
type Access struct {
	ID        primitive.ObjectID  `bson:"_id,omitempty"`
	UserID    primitive.ObjectID  `bson:"user_id"`
	CourseID  primitive.ObjectID  `bson:"course_id"`
	GrantedAt time.Time           `bson:"granted_at"`
	GrantedBy int64               `bson:"granted_by"`  // telegram admin user_id
	ExpiresAt *time.Time          `bson:"expires_at"`  // nil = permanent
}

// CachedFile — one Telegram message cached from channel
// Admin runs /syncchannel once; new uploads auto-append.
type CachedFile struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"  json:"id"`
	MessageID int                `bson:"message_id"     json:"message_id"`
	ChannelID int64              `bson:"channel_id"     json:"channel_id"`   // which TG channel this message is from
	CourseID  primitive.ObjectID `bson:"course_id"      json:"course_id"`
	Title     string             `bson:"title"          json:"title"`      // human-readable label (caption > filename)
	Caption   string             `bson:"caption"        json:"caption"`    // raw Telegram message caption
	FileName  string             `bson:"file_name"      json:"file_name"`
	MimeType  string             `bson:"mime_type"      json:"mime_type"`
	FileSize  int64              `bson:"file_size"      json:"file_size"`
	SortOrder int                `bson:"sort_order"     json:"sort_order"` // playback order inside course
	CreatedAt time.Time          `bson:"created_at"     json:"created_at"`
}

// Worker — an external streaming worker instance registered with the main server.
type Worker struct {
	ID       primitive.ObjectID `bson:"_id"`
	URL      string             `bson:"url"`       // "https://worker1.koyeb.app" (no trailing slash)
	Status   string             `bson:"status"`    // "active" | "unhealthy" | "offline"
	AddedBy  string             `bson:"added_by"`  // "env" | "admin"
	LastSeen time.Time          `bson:"last_seen"`
	AddedAt  time.Time          `bson:"added_at"`
}

// WatchHistory — tracks which files a user has viewed and how many times.
type WatchHistory struct {
	ID           primitive.ObjectID `bson:"_id,omitempty"      json:"id"`
	UserID       primitive.ObjectID `bson:"user_id"            json:"user_id"`
	MessageID    int                `bson:"message_id"         json:"message_id"`
	ChannelID    int64              `bson:"channel_id"         json:"channel_id"`
	CourseID     primitive.ObjectID `bson:"course_id"          json:"course_id"`
	ViewCount    int                `bson:"view_count"         json:"view_count"`
	LastViewedAt time.Time          `bson:"last_viewed_at"     json:"last_viewed_at"`
	// Resume position — user ने video कुठे सोडला ते track करतो
	WatchPosition          float64   `bson:"watch_position"            json:"watch_position"`
	WatchPositionUpdatedAt time.Time `bson:"watch_position_updated_at" json:"watch_position_updated_at"`
}

// Announcement — admin broadcast message shown as popup (global) or course banner (course-specific).
// CourseID = nil  → global announcement → shown as popup on /courses after login.
// CourseID = set  → course-specific     → shown as banner on /course/:id (access-gated).
type Announcement struct {
	ID        primitive.ObjectID  `bson:"_id,omitempty"  json:"id"`
	Title     string              `bson:"title"          json:"title"`
	Message   string              `bson:"message"        json:"message"`
	CourseID  *primitive.ObjectID `bson:"course_id"      json:"course_id"`  // nil = global
	CreatedAt time.Time           `bson:"created_at"     json:"created_at"`
	IsActive  bool                `bson:"is_active"      json:"is_active"`
}

// StreamNonce — per-page-load nonce that ties a player page to its stream.
type StreamNonce struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	Nonce      string             `bson:"nonce"`       // random UUID — goes in ?n= query param
	UserID     primitive.ObjectID `bson:"user_id"`
	WatchToken string             `bson:"watch_token"` // the /watch/:token value (for scoping)
	CreatedAt  time.Time          `bson:"created_at"`
	ExpiresAt  time.Time          `bson:"expires_at"`  // TTL index auto-deletes this
}

// FolderRange — एका folder मधला एक number range (e.g. 1–26)
// Start आणि End म्हणजे files चा sort_order (sequential number)
type FolderRange struct {
	Start int `bson:"start" json:"start"`
	End   int `bson:"end"   json:"end"`
}

// CourseFolder — course मधल्या files ला subject-wise group करतो.
// एक course मध्ये multiple folders असू शकतात.
// Ranges म्हणजे sort_order ranges — e.g. [{1,26}, {45,77}]
// Range बाहेरचे files "Ungrouped" म्हणून दाखवले जातात.
type CourseFolder struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CourseID  primitive.ObjectID `bson:"course_id"     json:"course_id"`
	Name      string             `bson:"name"          json:"name"`
	Ranges    []FolderRange      `bson:"ranges"        json:"ranges"`
	Messages  []int              `bson:"messages"      json:"messages"`   // individual sort_order numbers (outside ranges)
	Order     int                `bson:"order"         json:"order"`      // display order (1, 2, 3...)
	CreatedAt time.Time          `bson:"created_at"    json:"created_at"`
}

// Connect reads MONGODB_URI from env, connects, creates collections + indexes.
// Call this once at startup (before bot and HTTP server start).
func Connect() {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		log.Fatal("[db] MONGODB_URI env variable is not set")
	}

	dbName := os.Getenv("MONGODB_DB")
	if dbName == "" {
		dbName = "coursebot" // sensible default
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	clientOpts := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		log.Fatalf("[db] mongo.Connect error: %v", err)
	}

	// Ping to verify connection
	if err = client.Ping(ctx, nil); err != nil {
		log.Fatalf("[db] ping error: %v", err)
	}

	Client = client
	Database = client.Database(dbName)

	// Assign collection handles
	ColUsers        = Database.Collection("users")
	ColCourses      = Database.Collection("courses")
	ColAccess       = Database.Collection("access")
	ColSessions     = Database.Collection("sessions")
	ColChannelCache = Database.Collection("channel_cache")
	ColStreamNonces = Database.Collection("stream_nonces")
	ColWorkers      = Database.Collection("workers")
	ColOTPs         = Database.Collection("otp_records")             // ← OTP login records
	ColPendingRegs  = Database.Collection("pending_registrations")   // ← unverified registrations
	ColURLMonitors  = Database.Collection("url_monitors")            // ← URL monitor
	ColWatchHistory  = Database.Collection("watch_history")           // ← watch history
	ColAnnouncements = Database.Collection("announcements")           // ← broadcast announcements
	ColFolders       = Database.Collection("course_folders")          // ← course folders
	ColSettings      = Database.Collection("app_settings")            // ← key-value settings

	createIndexes()
	log.Printf("[db] connected to MongoDB — database: %s", dbName)
}

// ─── Indexes ─────────────────────────────────────────────────────────────────

func createIndexes() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// users: unique username, mobile, email (sparse — empty strings don't conflict)
	ensureIndex(ctx, ColUsers, bson.D{{Key: "username", Value: 1}}, true)
	ensureIndex(ctx, ColUsers, bson.D{{Key: "mobile", Value: 1}}, true)
	ensureIndex(ctx, ColUsers, bson.D{{Key: "email", Value: 1}}, true)

	// sessions: unique token + TTL auto-delete when expires_at passes
	ensureIndex(ctx, ColSessions, bson.D{{Key: "token", Value: 1}}, true)
	ensureIndex(ctx, ColSessions, bson.D{{Key: "user_id", Value: 1}}, false)
	ensureTTLIndex(ctx, ColSessions, "expires_at")

	// access: one grant per user+course pair
	ensureCompoundIndex(ctx, ColAccess,
		bson.D{{Key: "user_id", Value: 1}, {Key: "course_id", Value: 1}},
		true,
	)

	// channel_cache: fast lookup by (message_id, channel_id) — unique per channel
	ensureCompoundIndex(ctx, ColChannelCache,
		bson.D{{Key: "message_id", Value: 1}, {Key: "channel_id", Value: 1}},
		true,
	)
	ensureIndex(ctx, ColChannelCache, bson.D{{Key: "course_id", Value: 1}}, false)
	ensureIndex(ctx, ColChannelCache, bson.D{{Key: "sort_order", Value: 1}}, false)

	// stream_nonces: unique nonce + compound unique (user_id, watch_token) + TTL
	ensureIndex(ctx, ColStreamNonces, bson.D{{Key: "nonce", Value: 1}}, true)
	ensureCompoundIndex(ctx, ColStreamNonces,
		bson.D{{Key: "user_id", Value: 1}, {Key: "watch_token", Value: 1}},
		true,
	)
	ensureTTLIndex(ctx, ColStreamNonces, "expires_at")

	// workers: unique URL
	ensureIndex(ctx, ColWorkers, bson.D{{Key: "url", Value: 1}}, true)
	ensureIndex(ctx, ColWorkers, bson.D{{Key: "status", Value: 1}}, false)

	// otp_records: fast email lookup + TTL auto-delete on expires_at
	ensureIndex(ctx, ColOTPs, bson.D{{Key: "email", Value: 1}}, false)
	ensureTTLIndex(ctx, ColOTPs, "expires_at")

	ensureIndex(ctx, ColPendingRegs, bson.D{{Key: "email", Value: 1}}, false)
	ensureTTLIndex(ctx, ColPendingRegs, "expires_at")

	// url_monitors: unique URL — ek URL ek velach add hota yeil
	ensureIndex(ctx, ColURLMonitors, bson.D{{Key: "url", Value: 1}}, true)
	ensureIndex(ctx, ColURLMonitors, bson.D{{Key: "enabled", Value: 1}}, false)

	// watch_history: unique (user_id, message_id, channel_id) — one record per user per file
	ensureCompoundIndex(ctx, ColWatchHistory,
		bson.D{{Key: "user_id", Value: 1}, {Key: "message_id", Value: 1}, {Key: "channel_id", Value: 1}},
		true,
	)
	ensureIndex(ctx, ColWatchHistory, bson.D{{Key: "user_id", Value: 1}}, false)
	ensureIndex(ctx, ColWatchHistory, bson.D{{Key: "course_id", Value: 1}}, false)
	ensureIndex(ctx, ColWatchHistory, bson.D{{Key: "last_viewed_at", Value: -1}}, false)

	// Announcements indexes
	ensureIndex(ctx, ColAnnouncements, bson.D{{Key: "course_id", Value: 1}}, false)
	ensureIndex(ctx, ColAnnouncements, bson.D{{Key: "is_active", Value: 1}}, false)
	ensureIndex(ctx, ColAnnouncements, bson.D{{Key: "created_at", Value: -1}}, false)

	// users: telegram_id for /dl bot command lookup
	// Drop first to handle conflict with any previously created unique index
	_, _ = ColUsers.Indexes().DropOne(ctx, "telegram_id_1")
	ensureIndex(ctx, ColUsers, bson.D{{Key: "telegram_id", Value: 1}}, false)

	// offline_tokens: unique token + TTL (auto-delete at midnight IST via expires_at)
	ensureIndex(ctx, Database.Collection("offline_tokens"), bson.D{{Key: "token", Value: 1}}, true)
	ensureIndex(ctx, Database.Collection("offline_tokens"), bson.D{{Key: "user_id", Value: 1}}, false)
	ensureTTLIndex(ctx, Database.Collection("offline_tokens"), "expires_at")

	// offline_daily_usage: unique (user_id, date_key) — one counter per user per day
	ensureCompoundIndex(ctx, Database.Collection("offline_daily_usage"),
		bson.D{{Key: "user_id", Value: 1}, {Key: "date_key", Value: 1}},
		true,
	)

	// pending_deletes: messages scheduled for auto-delete after 48h
	ensureIndex(ctx, Database.Collection("pending_deletes"), bson.D{{Key: "delete_at", Value: 1}}, false)
	ensureIndex(ctx, Database.Collection("pending_deletes"), bson.D{{Key: "deleted", Value: 1}}, false)

	// user_guard: suspicion score + block status per user
	ensureIndex(ctx, Database.Collection("user_guard"), bson.D{{Key: "user_id", Value: 1}}, true)
	ensureIndex(ctx, Database.Collection("user_guard"), bson.D{{Key: "suspicion_score", Value: -1}}, false)
	ensureIndex(ctx, Database.Collection("user_guard"), bson.D{{Key: "is_blocked", Value: 1}}, false)

	// course_folders: fast lookup by course_id, ordered by display order
	ensureIndex(ctx, ColFolders, bson.D{{Key: "course_id", Value: 1}}, false)
	ensureIndex(ctx, ColFolders, bson.D{{Key: "order", Value: 1}}, false)
}

func ensureIndex(ctx context.Context, col *mongo.Collection, keys bson.D, unique bool) {
	model := mongo.IndexModel{
		Keys:    keys,
		Options: options.Index().SetUnique(unique).SetSparse(true),
	}
	if _, err := col.Indexes().CreateOne(ctx, model); err != nil {
		log.Printf("[db] index warning (%s): %v", col.Name(), err)
	}
}

func ensureCompoundIndex(ctx context.Context, col *mongo.Collection, keys bson.D, unique bool) {
	model := mongo.IndexModel{
		Keys:    keys,
		Options: options.Index().SetUnique(unique),
	}
	if _, err := col.Indexes().CreateOne(ctx, model); err != nil {
		log.Printf("[db] compound index warning (%s): %v", col.Name(), err)
	}
}

func ensureTTLIndex(ctx context.Context, col *mongo.Collection, field string) {
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: field, Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0), // MongoDB deletes at expires_at value
	}
	if _, err := col.Indexes().CreateOne(ctx, model); err != nil {
		log.Printf("[db] TTL index warning (%s.%s): %v", col.Name(), field, err)
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// Ctx returns a 10-second background context for DB ops.
func Ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// Disconnect cleanly closes the MongoDB connection (call on shutdown).
func Disconnect() {
	if Client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Client.Disconnect(ctx); err != nil {
		log.Printf("[db] disconnect error: %v", err)
	}
}
