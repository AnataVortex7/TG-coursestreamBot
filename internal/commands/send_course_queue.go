// internal/commands/send_course_queue.go
//
// Admin panel varun user la tyacha course Telegram var SEND karnyasathi.
//
//	SendCourseToUser(userID, courseID)
//	  ├─ user Telegram-linked asel ani bot ne tyacha chat baghitla asel (user ne bot var
//	  │    kadhi msg kela asel)           → lagech background madhe lectures pathavto
//	  └─ nahi (user ne ajun bot var msg kela nahi / link zala nahi)
//	                                      → `pending_course_sends` madhe queue karto.
//
//	StartPendingCourseSend — background worker (dar 20 sec). User ne bot var msg takla,
//	Verification (number share) purna zala ani account link zala ki queue madhle
//	courses apoaap pathavto. Admin la parat kahi karayla nako.
//
// Registration flow (contact_gate.go): admin ne panel varun user banavla (mobile sahit)
// tar user bot var kuthlahi msg takto -> popup -> number share -> mobile DB madhe sapadla
// -> AUTO-LINK. Mag he worker course pathavto.
package commands

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

const (
	pcsTickEvery   = 20 * time.Second
	pcsMaxAttempts = 5
)

var (
	pcsMu       sync.Mutex
	pcsInFlight = map[string]bool{} // "<userHex>:<courseHex>" — ekach velach ekach send
)

func pcsKey(userID, courseID primitive.ObjectID) string {
	return userID.Hex() + ":" + courseID.Hex()
}

func pcsAcquire(key string) bool {
	pcsMu.Lock()
	defer pcsMu.Unlock()
	if pcsInFlight[key] {
		return false
	}
	pcsInFlight[key] = true
	return true
}

func pcsRelease(key string) {
	pcsMu.Lock()
	delete(pcsInFlight, key)
	pcsMu.Unlock()
}

// userReachable — user Telegram-linked aahe ani bot ne tyacha chat (peer) baghitla aahe ka.
func userReachable(botClient *gotgproto.Client, user *db.User) bool {
	if botClient == nil || botClient.Self == nil || user == nil || user.TelegramID == 0 {
		return false
	}
	gctx := botClient.CreateContext()
	return !gctx.PeerStorage.GetInputPeerById(user.TelegramID).Zero()
}

// SendCourseResult — admin API cha response.
type SendCourseResult struct {
	Queued   bool `json:"queued"`   // true = user ne ajun bot var msg kela nahi; link zalyavar apoaap jail
	Lectures int  `json:"lectures"` // course madhle ekun lectures
}

// SendCourseToUser — admin panel varun call hoto (routes/admin_api.go).
func SendCourseToUser(userID, courseID primitive.ObjectID) (*SendCourseResult, error) {
	user, err := db.GetUserByID(userID)
	if err != nil || user == nil {
		return nil, errors.New("User sapadla nahi")
	}
	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		return nil, errors.New("Course sapadla nahi")
	}
	if has, _ := db.HasAccess(userID, courseID); !has {
		return nil, errors.New("Ya user kade ya course cha access nahi — aadhi Grant Access kara")
	}
	files, err := db.FilesForCourse(courseID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("Ya course madhe kahich lectures nahit")
	}
	botClient := MainBotClient
	if botClient == nil || botClient.Self == nil {
		return nil, errors.New("Bot client chalu nahi")
	}

	// User ne ajun bot var msg kela nahi / link nahi → queue
	if !userReachable(botClient, user) {
		if err := db.QueuePendingCourseSend(userID, courseID); err != nil {
			return nil, err
		}
		return &SendCourseResult{Queued: true, Lectures: len(files)}, nil
	}

	key := pcsKey(userID, courseID)
	if !pcsAcquire(key) {
		return nil, errors.New("Ha course ya user la aadhich pathavla jat aahe — thoda thamba")
	}
	go func() {
		defer pcsRelease(key)
		sent, failed := runCourseSend(botClient, user, course, courseID)
		msg := fmt.Sprintf("✅ Admin panel: %s la '%s' cha %d lectures pathavle.", user.FullName, course.Title, sent)
		if failed > 0 {
			msg += fmt.Sprintf("\n⚠️ %d ayashasvi — panel varun parat Send kara (cache mule aadhiche lagech jatil).", failed)
		}
		if NotifyAdmins != nil {
			NotifyAdmins(msg)
		}
	}()
	return &SendCourseResult{Queued: false, Lectures: len(files)}, nil
}

// runCourseSend — course che sarv lectures user chya Telegram chat madhe pathavto (blocking).
func runCourseSend(botClient *gotgproto.Client, user *db.User, course *db.Course, courseID primitive.ObjectID) (sent, failed int) {
	files, err := db.FilesForCourse(courseID)
	if err != nil || len(files) == 0 {
		return 0, 0
	}
	gctx := botClient.CreateContext()
	bg := context.Background()

	// User la ek chota heads-up (best-effort)
	if peer := gctx.PeerStorage.GetInputPeerById(user.TelegramID); !peer.Zero() {
		_, _ = botClient.API().MessagesSendMessage(bg, &tg.MessagesSendMessageRequest{
			Peer: peer,
			Message: fmt.Sprintf("📚 '%s' course che %d lectures pathavayla survat hot aahe.\n"+
				"Ekamagun ek yetil — kripaya thamba.", course.Title, len(files)),
			RandomID: rand.Int63(),
		})
	}

	for _, f := range files {
		ok, dErr := runWithFloodRetryGeneric(bg, 5, func() (bool, error) {
			// cachedOnly=true: watermarked copy LOG_CHANNEL cache madhe asel tar tich forward hote;
			// nasel tar original (unwatermarked) copy lagech jate ani "raw" mhanun track hote.
			// Nantar /predownload (watermark worker) ne cache bharla ki auto-delivery cha
			// adResolvePending (dar ~30s) tya SAME message la watermarked video ne EDIT/replace karto.
			return deliverLectureCoreOpt(gctx, botClient, user, course, courseID,
				f.MessageID, user.TelegramID, 0, false, func(string) {}, true)
		})
		if dErr != nil || !ok {
			failed++
		} else {
			sent++
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return sent, failed
}

// StartPendingCourseSend — main.go / run.go madhun `go` ne chalva.
func StartPendingCourseSend(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	log = log.Named("pending_course_send")
	log.Info("Pending course-send worker started")

	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second): // bot ani peer cache tayar hou dya
	}

	t := time.NewTicker(pcsTickEvery)
	defer t.Stop()
	for {
		pcsTick(ctx, client, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func pcsTick(ctx context.Context, client *gotgproto.Client, log *zap.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Sugar().Errorw("pending_course_send panic recovered", "panic", r)
		}
	}()
	if client == nil || client.Self == nil || db.Database == nil {
		return
	}
	list, err := db.ListPendingCourseSends()
	if err != nil || len(list) == 0 {
		return
	}

	for _, p := range list {
		if ctx.Err() != nil {
			return
		}
		user, uErr := db.GetUserByID(p.UserID)
		if uErr != nil {
			continue // DB error — pudhchya tick la
		}
		if user == nil { // user delete zala
			_ = db.RemovePendingCourseSend(p.ID)
			continue
		}
		course, cErr := db.GetCourseByID(p.CourseID)
		if cErr != nil {
			continue
		}
		if course == nil { // course delete zala
			_ = db.RemovePendingCourseSend(p.ID)
			continue
		}
		has, aErr := db.HasAccess(p.UserID, p.CourseID)
		if aErr != nil {
			continue
		}
		if !has { // access revoke/expire zala
			_ = db.RemovePendingCourseSend(p.ID)
			continue
		}
		if blocked, _, _ := db.IsUserBlocked(p.UserID); blocked {
			continue
		}
		if !userReachable(client, user) {
			continue // user ne ajun bot var msg/verification kela nahi — thamba
		}

		key := pcsKey(p.UserID, p.CourseID)
		if !pcsAcquire(key) {
			continue
		}
		sent, failed := runCourseSend(client, user, course, p.CourseID)
		pcsRelease(key)

		if failed == 0 {
			_ = db.RemovePendingCourseSend(p.ID)
			if NotifyAdmins != nil {
				NotifyAdmins(fmt.Sprintf("✅ Queued course delivered: %s ← '%s' (%d lectures)", user.FullName, course.Title, sent))
			}
			continue
		}
		if n := db.IncPendingCourseSendAttempt(p.ID); n >= pcsMaxAttempts {
			_ = db.RemovePendingCourseSend(p.ID)
			if NotifyAdmins != nil {
				NotifyAdmins(fmt.Sprintf("⚠️ Queued course send ayashasvi (%d prayatna): %s ← '%s'. Panel varun parat Send kara.",
					n, user.FullName, course.Title))
			}
		}
	}
}
