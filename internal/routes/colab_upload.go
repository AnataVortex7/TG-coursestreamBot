// internal/routes/colab_upload.go
// ── Colab Chunked Upload Receive — DEPRECATED ────────────────────────────────
//
// आधी: Colab FFmpeg done → processed file chunks मध्ये Koyeb ला POST →
//      reassemble → MTProto ne Telegram send.
//
// आता: Colab स्वतः processed file थेट Telegram Bot API (sendVideo) ने
//      target chat ला पाठवतो (job.bot_token + job.send_to_chat_id, जे
//      /api/colab/poll response मध्ये already असतात).
//      Koyeb ला file कधीच परत येत नाही — chunk/finalize routes यापुढे
//      वापरले जात नाहीत.
//
// हा file फक्त LoadColabUpload (reflection-based route loader) साठी
// stub म्हणून ठेवला आहे — कुठलेही routes register होत नाहीत.

package routes

func (e *allRoutes) LoadColabUpload(r *Route) {
	// No-op — chunked upload routes काढले.
	// Colab आता Bot API ने थेट Telegram ला send करतो (Koyeb hop नाही).
}
