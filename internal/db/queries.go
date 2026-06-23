package db

import (
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ═══════════════════════════════════════════════════════════════════════════════
// USERS
// ═══════════════════════════════════════════════════════════════════════════════

func CreateUser(username, passwordHash string) (*User, error) {
	return CreateUserFull(username, "", "", "", passwordHash)
}

// CreateUserFull creates a user with optional fullName, mobile, email.
// username is auto-derived from mobile or email if blank.
// Returns error if username/mobile/email already taken.
func CreateUserFull(username, fullName, mobile, email, passwordHash string) (*User, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Auto-derive username if not provided
	if username == "" {
		if mobile != "" {
			username = mobile
		} else if email != "" {
			username = email
		}
	}

	user := &User{
		ID:           primitive.NewObjectID(),
		Username:     username,
		FullName:     fullName,
		Mobile:       mobile,
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now(),
		IsActive:     true,
	}
	_, err := ColUsers.InsertOne(ctx, user)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, errors.New("mobile number or email already registered")
		}
		return nil, err
	}
	return user, nil
}

// GetUserByMobileOrEmail looks up a user by mobile number or email address.
// Used for login when user types their mobile/email instead of username.
func GetUserByMobileOrEmail(identifier string) (*User, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var user User
	err := ColUsers.FindOne(ctx, bson.M{
		"$or": bson.A{
			bson.M{"mobile": identifier},
			bson.M{"email": identifier},
			bson.M{"username": identifier},
		},
	}).Decode(&user)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

func GetUserByUsername(username string) (*User, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var user User
	err := ColUsers.FindOne(ctx, bson.M{"username": username}).Decode(&user)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil // not found — caller checks nil
		}
		return nil, err
	}
	return &user, nil
}

func GetUserByID(id primitive.ObjectID) (*User, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var user User
	err := ColUsers.FindOne(ctx, bson.M{"_id": id}).Decode(&user)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

func SetUserActive(id primitive.ObjectID, active bool) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColUsers.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"is_active": active}},
	)
	return err
}

// SetUserSpecial — admin panel cha "Special User" toggle. ON kelyavar user la
// not-buyed courses cha content list (titles) disel, pan play/download tevha
// pn HasAccess (purchase/admin grant) shivay milnar nahi.
func SetUserSpecial(id primitive.ObjectID, special bool) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColUsers.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"is_special_user": special}},
	)
	return err
}

// ─── Device Fingerprint ───────────────────────────────────────────────────────

// SetUserFingerprint — successful login var first time kiwa 24h window pass
// zali ki nava fingerprint save karto aani pending session-delete clear karto.
func SetUserFingerprint(id primitive.ObjectID, fp string) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColUsers.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{
			"device_fingerprint": fp,
			"session_delete_at":  nil,
		}},
	)
	return err
}

// RequestSessionDelete — user ne "24 tasant session delete kara" button dabla.
// at = time.Now().Add(24 * time.Hour).
func RequestSessionDelete(id primitive.ObjectID, at time.Time) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColUsers.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"session_delete_at": at}},
	)
	return err
}

// ClearUserDevice — admin /kill command. Fingerprint + pending delete turant
// clear karto; tyanantar kuthlyahi device varun login allow hoto.
func ClearUserDevice(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColUsers.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{
			"device_fingerprint": "",
			"session_delete_at":  nil,
		}},
	)
	return err
}

func ListUsers() ([]User, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColUsers.Find(ctx, bson.M{}, options.Find().SetSort(bson.M{"created_at": -1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var users []User
	if err = cursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// SESSIONS  (one active session per user — new login kills old)
// ═══════════════════════════════════════════════════════════════════════════════

// UpsertSession deletes any existing session for userID then inserts the new one.
// This enforces the "one session at a time" rule at the DB level.
func UpsertSession(userID primitive.ObjectID, token, ip, userAgent string, expiresAt time.Time) error {
	ctx, cancel := Ctx()
	defer cancel()

	// Delete all previous sessions for this user
	if _, err := ColSessions.DeleteMany(ctx, bson.M{"user_id": userID}); err != nil {
		return err
	}

	session := &Session{
		ID:        primitive.NewObjectID(),
		UserID:    userID,
		Token:     token,
		IP:        ip,
		UserAgent: userAgent,
		CreatedAt: time.Now(),
		ExpiresAt: expiresAt,
	}
	_, err := ColSessions.InsertOne(ctx, session)
	return err
}

func GetSessionByToken(token string) (*Session, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var s Session
	err := ColSessions.FindOne(ctx, bson.M{"token": token}).Decode(&s)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func DeleteSession(token string) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColSessions.DeleteOne(ctx, bson.M{"token": token})
	return err
}

func DeleteAllSessionsForUser(userID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColSessions.DeleteMany(ctx, bson.M{"user_id": userID})
	return err
}

func ListActiveSessions() ([]Session, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Fakt non-expired sessions (expired sessions TTL janitor kadhto, pan
	// refresh interval madhe diste rahtat — tya exclude karto taki Kill
	// button meaningful rahil).
	cursor, err := ColSessions.Find(ctx,
		bson.M{"expires_at": bson.M{"$gt": time.Now()}},
		options.Find().SetSort(bson.M{"created_at": -1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var sessions []Session
	if err = cursor.All(ctx, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// COURSES
// ═══════════════════════════════════════════════════════════════════════════════

func CreateCourse(title, description, thumbnail, price, buyLink string, channelID int64) (*Course, error) {
	ctx, cancel := Ctx()
	defer cancel()

	course := &Course{
		ID:          primitive.NewObjectID(),
		Title:       title,
		Description: description,
		Thumbnail:   thumbnail,
		Price:       price,
		BuyLink:     buyLink,
		ChannelID:   channelID,
		IsActive:    true,
		CreatedAt:   time.Now(),
	}
	_, err := ColCourses.InsertOne(ctx, course)
	if err != nil {
		return nil, err
	}
	return course, nil
}


// GetCourseByChannelID — channel_id वरून active course शोधतो.
// Auto-cache handler वापरतो — नवीन channel message आल्यावर course identify करायला.
func GetCourseByChannelID(channelID int64) (*Course, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var course Course
	err := ColCourses.FindOne(ctx, bson.M{"channel_id": channelID, "is_active": true}).Decode(&course)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &course, nil
}

func GetCourseByID(id primitive.ObjectID) (*Course, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var course Course
	err := ColCourses.FindOne(ctx, bson.M{"_id": id, "is_active": true}).Decode(&course)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &course, nil
}

func ListAllCourses() ([]Course, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColCourses.Find(ctx, bson.M{"is_active": true},
		options.Find().SetSort(bson.M{"created_at": -1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var courses []Course
	if err = cursor.All(ctx, &courses); err != nil {
		return nil, err
	}
	return courses, nil
}

func UpdateCourse(id primitive.ObjectID, fields bson.M) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColCourses.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields})
	return err
}

func DeleteCourse(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	// soft delete
	_, err := ColCourses.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"is_active": false}},
	)
	return err
}

// ═══════════════════════════════════════════════════════════════════════════════
// ACCESS  (user ↔ course mapping — no expiry)
// ═══════════════════════════════════════════════════════════════════════════════

// GrantAccess gives a user access to a course.
// months=0 means permanent access; months>0 sets an expiry date.
func GrantAccess(userID, courseID primitive.ObjectID, adminTelegramID int64, months int) error {
	ctx, cancel := Ctx()
	defer cancel()

	var expiresAt *time.Time
	if months > 0 {
		t := time.Now().AddDate(0, months, 0)
		expiresAt = &t
	}

	// Upsert: if access already exists, update the expiry
	filter := bson.M{"user_id": userID, "course_id": courseID}
	update := bson.M{"$set": bson.M{
		"granted_at": time.Now(),
		"granted_by": adminTelegramID,
		"expires_at": expiresAt,
	}}
	opts := options.Update().SetUpsert(true)
	_, err := ColAccess.UpdateOne(ctx, filter, update, opts)
	return err
}

func RevokeAccess(userID, courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColAccess.DeleteOne(ctx, bson.M{"user_id": userID, "course_id": courseID})
	return err
}

func HasAccess(userID, courseID primitive.ObjectID) (bool, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Valid if: record exists AND (expires_at is null OR expires_at > now)
	filter := bson.M{
		"user_id":   userID,
		"course_id": courseID,
		"$or": bson.A{
			bson.M{"expires_at": nil},
			bson.M{"expires_at": bson.M{"$gt": time.Now()}},
		},
	}
	count, err := ColAccess.CountDocuments(ctx, filter)
	return count > 0, err
}

// ActiveAccessForUser returns full Access records (with expiry) for a user.
// Only returns non-expired records.
func ActiveAccessForUser(userID primitive.ObjectID) ([]Access, error) {
	ctx, cancel := Ctx()
	defer cancel()

	filter := bson.M{
		"user_id": userID,
		"$or": bson.A{
			bson.M{"expires_at": nil},
			bson.M{"expires_at": bson.M{"$gt": time.Now()}},
		},
	}
	cursor, err := ColAccess.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var records []Access
	if err = cursor.All(ctx, &records); err != nil {
		return nil, err
	}
	return records, nil
}

// CoursesForUser returns all course IDs a user has access to.
func CoursesForUser(userID primitive.ObjectID) ([]primitive.ObjectID, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Only return non-expired access records
	filter := bson.M{
		"user_id": userID,
		"$or": bson.A{
			bson.M{"expires_at": nil},
			bson.M{"expires_at": bson.M{"$gt": time.Now()}},
		},
	}
	cursor, err := ColAccess.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var records []Access
	if err = cursor.All(ctx, &records); err != nil {
		return nil, err
	}

	ids := make([]primitive.ObjectID, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.CourseID)
	}
	return ids, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// CHANNEL CACHE  (Telegram channel messages stored once, served forever)
// ═══════════════════════════════════════════════════════════════════════════════

// UpsertCachedFile inserts or updates a cached file by (message_id, channel_id).
// Different courses on different channels can share the same message_id.
func UpsertCachedFile(f *CachedFile) error {
	ctx, cancel := Ctx()
	defer cancel()

	filter := bson.M{"message_id": f.MessageID, "channel_id": f.ChannelID}
	update := bson.M{"$set": bson.M{
		"course_id":  f.CourseID,
		"title":      f.Title,
		"caption":    f.Caption,
		"file_name":  f.FileName,
		"mime_type":  f.MimeType,
		"file_size":  f.FileSize,
		"sort_order": f.SortOrder,
		"created_at": f.CreatedAt,
	}}
	opts := options.Update().SetUpsert(true)
	_, err := ColChannelCache.UpdateOne(ctx, filter, update, opts)
	return err
}

// GetCachedFile returns one cached file by (message_id, channel_id).
// channelID is required since the same message_id can exist in multiple channels.
func GetCachedFile(messageID int, channelID int64) (*CachedFile, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var f CachedFile
	err := ColChannelCache.FindOne(ctx, bson.M{"message_id": messageID, "channel_id": channelID}).Decode(&f)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &f, nil
}

// FilesForCourse returns all cached files for a course, ordered by sort_order.
func FilesForCourse(courseID primitive.ObjectID) ([]CachedFile, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColChannelCache.Find(ctx,
		bson.M{"course_id": courseID},
		options.Find().SetSort(bson.M{"sort_order": 1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var files []CachedFile
	if err = cursor.All(ctx, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// CountCachedFiles returns total cached files for a course.
func CountCachedFiles(courseID primitive.ObjectID) (int64, error) {
	ctx, cancel := Ctx()
	defer cancel()

	return ColChannelCache.CountDocuments(ctx, bson.M{"course_id": courseID})
}

// DeleteCachedFilesForCourse removes all cached files when a course is deleted.
func DeleteCachedFilesForCourse(courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColChannelCache.DeleteMany(ctx, bson.M{"course_id": courseID})
	return err
}

// caseInsensitiveReplaceAll — strings.ReplaceAll cha case-insensitive version,
// match cha original casing replace hoто (text madhla rest tasach rahतो).
func caseInsensitiveReplaceAll(s, find, replace string) (string, bool) {
	if s == "" || find == "" {
		return s, false
	}
	lowerS := strings.ToLower(s)
	lowerFind := strings.ToLower(find)
	if !strings.Contains(lowerS, lowerFind) {
		return s, false
	}
	var sb strings.Builder
	rest, lowerRest := s, lowerS
	for {
		idx := strings.Index(lowerRest, lowerFind)
		if idx == -1 {
			sb.WriteString(rest)
			break
		}
		sb.WriteString(rest[:idx])
		sb.WriteString(replace)
		rest = rest[idx+len(find):]
		lowerRest = lowerRest[idx+len(find):]
	}
	return sb.String(), true
}

// BulkReplaceInCachedFiles — course chya SAGLE cached files (title/caption/
// file_name, jya fields specify kelya tya) madhe find->replace karto. EKACH
// vели multiple files update hotat (round-robin/one-by-one navhe — sagle
// matching files ekaच call madhe).
//
// dryRun=true asel tar kahihi DB madhe save hot nahi — fakt kiti files match
// hotील ani 5 examples (before → after) return hotat, taki admin la apply
// karण्याआधी preview disel.
func BulkReplaceInCachedFiles(courseID primitive.ObjectID, find, replace string, fields []string, caseSensitive, dryRun bool) (matchedCount int, modifiedCount int, examples []string, err error) {
	files, ferr := FilesForCourse(courseID)
	if ferr != nil {
		return 0, 0, nil, ferr
	}

	fieldSet := map[string]bool{}
	for _, f := range fields {
		fieldSet[f] = true
	}

	replaceFn := func(s string) (string, bool) {
		if s == "" || find == "" {
			return s, false
		}
		if caseSensitive {
			if !strings.Contains(s, find) {
				return s, false
			}
			return strings.ReplaceAll(s, find, replace), true
		}
		return caseInsensitiveReplaceAll(s, find, replace)
	}

	var writes []mongo.WriteModel
	for _, f := range files {
		newTitle, titleChanged := f.Title, false
		newCaption, captionChanged := f.Caption, false
		newFileName, fileNameChanged := f.FileName, false

		if fieldSet["title"] {
			if nt, ok := replaceFn(f.Title); ok {
				newTitle, titleChanged = nt, true
			}
		}
		if fieldSet["caption"] {
			if nc, ok := replaceFn(f.Caption); ok {
				newCaption, captionChanged = nc, true
			}
		}
		if fieldSet["file_name"] {
			if nf, ok := replaceFn(f.FileName); ok {
				newFileName, fileNameChanged = nf, true
			}
		}

		if !titleChanged && !captionChanged && !fileNameChanged {
			continue
		}
		matchedCount++

		if len(examples) < 5 {
			before, after := f.Title, newTitle
			if captionChanged {
				before, after = f.Caption, newCaption
			} else if fileNameChanged {
				before, after = f.FileName, newFileName
			}
			examples = append(examples, before+"  →  "+after)
		}

		if dryRun {
			continue
		}

		setFields := bson.M{}
		if titleChanged {
			setFields["title"] = newTitle
		}
		if captionChanged {
			setFields["caption"] = newCaption
		}
		if fileNameChanged {
			setFields["file_name"] = newFileName
		}
		writes = append(writes, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": f.ID}).
			SetUpdate(bson.M{"$set": setFields}),
		)
	}

	if dryRun || len(writes) == 0 {
		return matchedCount, 0, examples, nil
	}

	ctx, cancel := Ctx()
	defer cancel()
	result, werr := ColChannelCache.BulkWrite(ctx, writes)
	if werr != nil {
		return matchedCount, 0, examples, werr
	}
	return matchedCount, int(result.ModifiedCount), examples, nil
}

// HighestSortOrder returns the current max sort_order for a course.
// New files are appended after this value.
func HighestSortOrder(courseID primitive.ObjectID) (int, error) {
	ctx, cancel := Ctx()
	defer cancel()

	opts := options.FindOne().SetSort(bson.M{"sort_order": -1})
	var f CachedFile
	err := ColChannelCache.FindOne(ctx, bson.M{"course_id": courseID}, opts).Decode(&f)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, err
	}
	return f.SortOrder, nil
}
// ═══════════════════════════════════════════════════════════════════════════════
// STREAM NONCES  (anti-piracy: per-page-load nonce binding)
// ═══════════════════════════════════════════════════════════════════════════════

// UpsertStreamNonce stores a fresh nonce for (userID, watchToken).
// Any existing nonce for that pair is replaced (upsert), so opening the player
// in a new tab immediately invalidates the previous tab's stream URL.
func UpsertStreamNonce(userID primitive.ObjectID, watchToken, nonce string, expiresAt time.Time) error {
	ctx, cancel := Ctx()
	defer cancel()

	filter := bson.M{"user_id": userID, "watch_token": watchToken}
	update := bson.M{
		"$set": bson.M{
			"nonce":      nonce,
			"user_id":    userID,
			"watch_token": watchToken,
			"created_at": time.Now(),
			"expires_at": expiresAt,
		},
	}
	opts := options.Update().SetUpsert(true)
	_, err := ColStreamNonces.UpdateOne(ctx, filter, update, opts)
	return err
}

// GetStreamNonce fetches a nonce document by the nonce string itself.
// Returns nil, nil when the nonce does not exist (expired or never issued).
func GetStreamNonce(nonce string) (*StreamNonce, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var sn StreamNonce
	err := ColStreamNonces.FindOne(ctx, bson.M{"nonce": nonce}).Decode(&sn)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &sn, nil
}

// ConsumeStreamNonce deletes a nonce from DB after first successful stream validation.
// This ensures the nonce is one-time-use from DB perspective.
// In-memory nonceStreamKeys still valid for seek/range requests within same session.
func ConsumeStreamNonce(nonce string) error {
	ctx, cancel := Ctx()
	defer cancel()
	_, err := ColStreamNonces.DeleteOne(ctx, bson.M{"nonce": nonce})
	return err
}

// ═══════════════════════════════════════════════════════════════════════════════
// WORKERS  (external streaming worker instances)
// ═══════════════════════════════════════════════════════════════════════════════

// UpsertWorker inserts or updates a worker by URL.
// On insert: sets status="active", added_at=now, last_seen=now, added_by=addedBy.
// On update: refreshes last_seen and status. added_by is preserved ($setOnInsert only).
func UpsertWorker(url, addedBy string) error {
	ctx, cancel := Ctx()
	defer cancel()

	now := time.Now()
	filter := bson.M{"url": url}
	update := bson.M{
		"$set": bson.M{
			"url":       url,
			"status":    "active",
			"last_seen": now,
		},
		"$setOnInsert": bson.M{
			"_id":      primitive.NewObjectID(),
			"added_at": now,
			"added_by": addedBy,
		},
	}
	opts := options.Update().SetUpsert(true)
	_, err := ColWorkers.UpdateOne(ctx, filter, update, opts)
	return err
}

// UpdateWorkerStatus sets the status field for a worker identified by URL.
// status should be "active", "unhealthy", or "offline".
func UpdateWorkerStatus(url, status string) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColWorkers.UpdateOne(ctx,
		bson.M{"url": url},
		bson.M{"$set": bson.M{"status": status}},
	)
	return err
}

// UpdateWorkerLastSeen refreshes last_seen and ensures status is "active".
func UpdateWorkerLastSeen(url string) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColWorkers.UpdateOne(ctx,
		bson.M{"url": url},
		bson.M{"$set": bson.M{"last_seen": time.Now(), "status": "active"}},
	)
	return err
}

// GetActiveWorkers returns all workers with status="active".
func GetActiveWorkers() ([]*Worker, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColWorkers.Find(ctx, bson.M{"status": "active"})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var workers []*Worker
	if err = cursor.All(ctx, &workers); err != nil {
		return nil, err
	}
	return workers, nil
}

// GetAllWorkers returns every worker document regardless of status.
func GetAllWorkers() ([]*Worker, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColWorkers.Find(ctx, bson.M{},
		options.Find().SetSort(bson.M{"added_at": -1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var workers []*Worker
	if err = cursor.All(ctx, &workers); err != nil {
		return nil, err
	}
	return workers, nil
}

// DeleteWorker removes a worker document by URL.
func DeleteWorker(url string) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColWorkers.DeleteOne(ctx, bson.M{"url": url})
	return err
}

// ═══════════════════════════════════════════════════════════════════════════════
// WATCH HISTORY  (per-user view tracking with count + last viewed time)
// ═══════════════════════════════════════════════════════════════════════════════

// RecordView upserts a watch history entry for (userID, messageID, channelID).
// On first view: creates record with view_count=1.
// On repeat view: increments view_count and updates last_viewed_at.
func RecordView(userID primitive.ObjectID, messageID int, channelID int64, courseID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	filter := bson.M{
		"user_id":    userID,
		"message_id": messageID,
		"channel_id": channelID,
	}
	update := bson.M{
		"$inc": bson.M{"view_count": 1},
		"$set": bson.M{
			"last_viewed_at": time.Now(),
			"course_id":      courseID,
		},
		"$setOnInsert": bson.M{
			"_id":     primitive.NewObjectID(),
			"user_id": userID,
		},
	}
	opts := options.Update().SetUpsert(true)
	_, err := ColWatchHistory.UpdateOne(ctx, filter, update, opts)
	return err
}

// GetWatchHistoryForCourse returns all WatchHistory records for a user in a specific course.
// Sorted by last_viewed_at descending (most recently watched first).
func GetWatchHistoryForCourse(userID, courseID primitive.ObjectID) ([]WatchHistory, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColWatchHistory.Find(ctx,
		bson.M{"user_id": userID, "course_id": courseID},
		options.Find().SetSort(bson.M{"last_viewed_at": -1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var records []WatchHistory
	if err = cursor.All(ctx, &records); err != nil {
		return nil, err
	}
	return records, nil
}

// CourseWatchSummary — admin chya "User Detail" page var, user ne kuthल्या
// courses madhe kiti lectures baghitले tyachi summary (drill-down list sathi).
type CourseWatchSummary struct {
	CourseID    primitive.ObjectID `json:"course_id"`
	CourseTitle string             `json:"course_title"`
	VideoCount  int                `json:"video_count"`
}

// GetUserWatchedCoursesSummary — user ne ज्या-ज्या courses madhe kahi baghitले
// aahe, tyanchi list + pratyekat kiti vegळी lectures baghitली ते count.
func GetUserWatchedCoursesSummary(userID primitive.ObjectID) ([]CourseWatchSummary, error) {
	ctx, cancel := Ctx()
	defer cancel()

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"user_id": userID}}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id":   "$course_id",
			"count": bson.M{"$sum": 1},
		}}},
		bson.D{{Key: "$sort", Value: bson.M{"count": -1}}},
	}
	cursor, err := ColWatchHistory.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var rows []struct {
		CourseID primitive.ObjectID `bson:"_id"`
		Count    int                `bson:"count"`
	}
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}

	result := make([]CourseWatchSummary, 0, len(rows))
	for _, r := range rows {
		title := "(deleted course)"
		if course, _ := GetCourseByID(r.CourseID); course != nil {
			title = course.Title
		}
		result = append(result, CourseWatchSummary{
			CourseID:    r.CourseID,
			CourseTitle: title,
			VideoCount:  r.Count,
		})
	}
	return result, nil
}

// CountDistinctVideosWatched — user ne aataparyant (sagle courses milun) kiti
// vegळी lectures baghitली - admin users-list cha "Videos Watched" overview badge.
func CountDistinctVideosWatched(userID primitive.ObjectID) (int64, error) {
	ctx, cancel := Ctx()
	defer cancel()
	return ColWatchHistory.CountDocuments(ctx, bson.M{"user_id": userID})
}

// CountDistinctVideosWatchedBulk — admin users-list sathi - EKACH aggregation
// call madhe SAGLYA users cha count milvto (N+1 query टाळण्यासाठी).
// Return: map[userIDHex]count
func CountDistinctVideosWatchedBulk() (map[string]int64, error) {
	ctx, cancel := Ctx()
	defer cancel()

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$group", Value: bson.M{
			"_id":   "$user_id",
			"count": bson.M{"$sum": 1},
		}}},
	}
	cursor, err := ColWatchHistory.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var rows []struct {
		UserID primitive.ObjectID `bson:"_id"`
		Count  int64              `bson:"count"`
	}
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}

	result := make(map[string]int64, len(rows))
	for _, r := range rows {
		result[r.UserID.Hex()] = r.Count
	}
	return result, nil
}

// GetFileViewCount returns the view_count and last_viewed_at for one specific file.
// Returns (0, zero-time, nil) if the user has never viewed the file.
func GetFileViewCount(userID primitive.ObjectID, messageID int, channelID int64) (int, time.Time, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var h WatchHistory
	err := ColWatchHistory.FindOne(ctx, bson.M{
		"user_id":    userID,
		"message_id": messageID,
		"channel_id": channelID,
	}).Decode(&h)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, time.Time{}, nil
		}
		return 0, time.Time{}, err
	}
	return h.ViewCount, h.LastViewedAt, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// ANNOUNCEMENTS  (admin broadcast → popup for global / banner for course)
// ═══════════════════════════════════════════════════════════════════════════════

// CreateAnnouncement inserts a new broadcast announcement.
// courseID = nil  → global (shows as popup on /courses after login).
// courseID = set  → course-specific (shows as banner on /course/:id, access-gated).
func CreateAnnouncement(title, message string, courseID *primitive.ObjectID) (*Announcement, error) {
	ctx, cancel := Ctx()
	defer cancel()

	a := &Announcement{
		ID:        primitive.NewObjectID(),
		Title:     title,
		Message:   message,
		CourseID:  courseID,
		CreatedAt: time.Now(),
		IsActive:  true,
	}
	_, err := ColAnnouncements.InsertOne(ctx, a)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// GetGlobalAnnouncements returns all active announcements with no course (global).
// Used for the popup on /courses page after login.
func GetGlobalAnnouncements() ([]Announcement, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColAnnouncements.Find(ctx,
		bson.M{"course_id": nil, "is_active": true},
		options.Find().SetSort(bson.M{"created_at": -1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []Announcement
	if err = cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// GetCourseAnnouncements returns active announcements for a specific course.
// Used for the banner on /course/:id page.
func GetCourseAnnouncements(courseID primitive.ObjectID) ([]Announcement, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColAnnouncements.Find(ctx,
		bson.M{"course_id": courseID, "is_active": true},
		options.Find().SetSort(bson.M{"created_at": -1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []Announcement
	if err = cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// GetUsersForCourse returns all User records who currently have active access to a course.
// Used by broadcast to find email targets for course-specific sends.
func GetUsersForCourse(courseID primitive.ObjectID) ([]User, error) {
	ctx, cancel := Ctx()
	defer cancel()

	// Step 1: find all active access records for this course
	filter := bson.M{
		"course_id": courseID,
		"$or": bson.A{
			bson.M{"expires_at": nil},
			bson.M{"expires_at": bson.M{"$gt": time.Now()}},
		},
	}
	cursor, err := ColAccess.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var records []Access
	if err = cursor.All(ctx, &records); err != nil {
		return nil, err
	}

	if len(records) == 0 {
		return nil, nil
	}

	// Step 2: collect user IDs
	userIDs := make([]primitive.ObjectID, 0, len(records))
	for _, r := range records {
		userIDs = append(userIDs, r.UserID)
	}

	// Step 3: fetch all matching users in one query
	userCursor, err := ColUsers.Find(ctx, bson.M{
		"_id":       bson.M{"$in": userIDs},
		"is_active": true,
	})
	if err != nil {
		return nil, err
	}
	defer userCursor.Close(ctx)

	var users []User
	if err = userCursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// DeleteAnnouncement permanently deletes an announcement from DB.
func DeleteAnnouncement(id primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColAnnouncements.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// ListAllAnnouncements returns all announcements (active + inactive) sorted newest first.
// Used in admin panel to manage broadcasts.
func ListAllAnnouncements() ([]Announcement, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColAnnouncements.Find(ctx,
		bson.M{},
		options.Find().SetSort(bson.M{"created_at": -1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []Announcement
	if err = cursor.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// ─── Telegram ID helpers (used by /dl bot command) ───────────────────────────

// GetUserByTelegramID finds a website user by their linked Telegram ID.
// Returns (nil, nil) if no user has this Telegram ID linked yet.
func GetUserByTelegramID(telegramID int64) (*User, error) {
	if telegramID == 0 {
		return nil, nil
	}
	ctx, cancel := Ctx()
	defer cancel()

	var u User
	err := ColUsers.FindOne(ctx, bson.M{"telegram_id": telegramID}).Decode(&u)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &u, nil
}

// SetUserTelegramID links a website user record to a Telegram user ID.
// Called by admin via the /settg bot command.
func SetUserTelegramID(userID primitive.ObjectID, telegramID int64) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColUsers.UpdateOne(ctx,
		bson.M{"_id": userID},
		bson.M{"$set": bson.M{"telegram_id": telegramID}},
	)
	return err
}

// ═══════════════════════════════════════════════════════════════════════════════
// WATCH POSITION  (resume playback)
// ═══════════════════════════════════════════════════════════════════════════════

// SaveWatchPosition — user ने video pause/exit केल्यावर position save करा.
// watch_history च्या existing record मध्येच update होतो — extra collection नाही.
// seconds < 5 असेल तर ignore (beginning).
// seconds >= 95% of duration असेल तर 0 save करतो (completed).
func SaveWatchPosition(userID primitive.ObjectID, messageID int, channelID int64, seconds float64) error {
	if seconds < 5 {
		return nil
	}
	ctx, cancel := Ctx()
	defer cancel()

	filter := bson.M{
		"user_id":    userID,
		"message_id": messageID,
		"channel_id": channelID,
	}
	update := bson.M{
		"$set": bson.M{
			"watch_position":            seconds,
			"watch_position_updated_at": time.Now(),
		},
		// Record नसेल तर insert होतो (RecordView नंतरच call होतो — record असेलच)
		"$setOnInsert": bson.M{
			"_id":     primitive.NewObjectID(),
			"user_id": userID,
		},
	}
	_, err := ColWatchHistory.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	return err
}

// GetWatchPosition — player load झाल्यावर saved position मिळव.
// 0 return = no saved position (first time / completed).
func GetWatchPosition(userID primitive.ObjectID, messageID int, channelID int64) (float64, error) {
	ctx, cancel := Ctx()
	defer cancel()

	var h WatchHistory
	err := ColWatchHistory.FindOne(ctx, bson.M{
		"user_id":    userID,
		"message_id": messageID,
		"channel_id": channelID,
	}).Decode(&h)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, err
	}
	return h.WatchPosition, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// USER PASSWORD UPDATE  (admin use only)
// ═══════════════════════════════════════════════════════════════════════════════

// UpdateUserPassword — admin कडून user चा password bcrypt hash ने update करतो.
// Plain password इथे येत नाही — caller ने आधीच hash करायला हवा.
func UpdateUserPassword(userID primitive.ObjectID, newPasswordHash string) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColUsers.UpdateOne(ctx,
		bson.M{"_id": userID},
		bson.M{"$set": bson.M{"password_hash": newPasswordHash}},
	)
	return err
}

// GetAllLinkedUsers — TelegramID linked असलेले सगळे active users returns करतो.
// Broadcast साठी वापरतो.
func GetAllLinkedUsers() ([]User, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColUsers.Find(ctx, bson.M{
		"telegram_id": bson.M{"$gt": 0},
		"is_active":   true,
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var users []User
	if err := cursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// APP SETTINGS — key-value toggles (admin panel वरून control होतात)
// ═══════════════════════════════════════════════════════════════════════════════

const SettingTelegramContact = "telegram_contact_enabled"

// GetSettingBool — key साठी bool value मिळवतो. Key नसेल तर defaultVal return करतो.
func GetSettingBool(key string, defaultVal bool) bool {
	ctx, cancel := Ctx()
	defer cancel()

	var doc struct {
		Value bool `bson:"value"`
	}
	err := ColSettings.FindOne(ctx, bson.M{"_id": key}).Decode(&doc)
	if err != nil {
		return defaultVal
	}
	return doc.Value
}

// SetSettingBool — key साठी bool value set/update करतो.
func SetSettingBool(key string, value bool) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColSettings.UpdateOne(ctx,
		bson.M{"_id": key},
		bson.M{"$set": bson.M{"value": value}},
		options.Update().SetUpsert(true),
	)
	return err
}

// ═══════════════════════════════════════════════════════════════════════════════
// ADMIN IMPERSONATE SESSION
// ═══════════════════════════════════════════════════════════════════════════════

// CreateImpersonateSession — UpsertSession सारखे पण user चा existing session
// delete करत नाही. Admin ला नवीन parallel session मिळतो; user logged out होत नाही.
func CreateImpersonateSession(userID primitive.ObjectID, token, ip, userAgent string, expiresAt time.Time) error {
	ctx, cancel := Ctx()
	defer cancel()

	session := &Session{
		ID:        primitive.NewObjectID(),
		UserID:    userID,
		Token:     token,
		IP:        ip,
		UserAgent: userAgent,
		CreatedAt: time.Now(),
		ExpiresAt: expiresAt,
	}
	_, err := ColSessions.InsertOne(ctx, session)
	return err
}

// ═══════════════════════════════════════════════════════════════════════════════
// EMAIL PROVIDER ASSIGNMENT (permanent, no round-robin)
// ═══════════════════════════════════════════════════════════════════════════════

// GetEmailProviderAssignment — email la aadhich kuthla provider assign zalay
// te baghto. found=false mhanje pahilyandach email pathvत aahot, navin
// assignment karaव lagel.
func GetEmailProviderAssignment(email string) (providerIndex int, found bool, err error) {
	ctx, cancel := Ctx()
	defer cancel()

	var rec EmailProviderAssignment
	dbErr := ColEmailProviderMap.FindOne(ctx, bson.M{"email": email}).Decode(&rec)
	if dbErr != nil {
		if errors.Is(dbErr, mongo.ErrNoDocuments) {
			return 0, false, nil
		}
		return 0, false, dbErr
	}
	return rec.ProviderIndex, true, nil
}

// CountEmailsForProvider — ya provider la aadhich kiti emails permanently
// assign zalet (capacity check sathi — max 7 per provider, last provider
// vagळaता).
func CountEmailsForProvider(providerIndex int) (int64, error) {
	ctx, cancel := Ctx()
	defer cancel()
	return ColEmailProviderMap.CountDocuments(ctx, bson.M{"provider_index": providerIndex})
}

// AssignEmailProvider — email la ek specific provider permanently assign
// karto. Upsert vaaparla aahe — jar dusarya goroutine ने tyाच vele tyач
// email sathi assignment kela asel (race condition), tar existing assignment
// retain hoto, duplicate/overwrite hoत nahi (email var unique index aahe).
func AssignEmailProvider(email string, providerIndex int) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColEmailProviderMap.UpdateOne(ctx,
		bson.M{"email": email},
		bson.M{"$setOnInsert": bson.M{
			"email":          email,
			"provider_index": providerIndex,
			"assigned_at":    time.Now(),
		}},
		options.Update().SetUpsert(true),
	)
	return err
}

// ═══════════════════════════════════════════════════════════════════════════════
// USER MESSAGES (admin → user, in-app + email)
// ═══════════════════════════════════════════════════════════════════════════════

// CreateUserMessage — admin ने message pathवल्यावर (email सोबत) save karto,
// taki user cha account madhe "Your Messages" tab madhe disel.
func CreateUserMessage(userID primitive.ObjectID, subject, message string) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColUserMessages.InsertOne(ctx, &UserMessage{
		ID:      primitive.NewObjectID(),
		UserID:  userID,
		Subject: subject,
		Message: message,
		SentAt:  time.Now(),
		ReadAt:  nil,
	})
	return err
}

// ListUserMessages — user chya sagle messages, newest first. User-facing
// "Your Messages" tab आणि admin chya "User Detail" page donhikade vaaparla
// jato.
func ListUserMessages(userID primitive.ObjectID) ([]UserMessage, error) {
	ctx, cancel := Ctx()
	defer cancel()

	cursor, err := ColUserMessages.Find(ctx,
		bson.M{"user_id": userID},
		options.Find().SetSort(bson.M{"sent_at": -1}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var msgs []UserMessage
	if err = cursor.All(ctx, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// CountUnreadUserMessages — nav bar cha red badge sathi.
func CountUnreadUserMessages(userID primitive.ObjectID) (int64, error) {
	ctx, cancel := Ctx()
	defer cancel()
	return ColUserMessages.CountDocuments(ctx, bson.M{
		"user_id": userID,
		"read_at": nil,
	})
}

// MarkAllUserMessagesRead — user ne messages list ughadli ki sagle unread
// messages "read" mhanun mark hotat (list ughadne = baghitले, simple rule).
func MarkAllUserMessagesRead(userID primitive.ObjectID) error {
	ctx, cancel := Ctx()
	defer cancel()

	_, err := ColUserMessages.UpdateMany(ctx,
		bson.M{"user_id": userID, "read_at": nil},
		bson.M{"$set": bson.M{"read_at": time.Now()}},
	)
	return err
}
