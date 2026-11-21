# GitHub Actions Workspaces (auto start/stop watermark workers)

## काय करतं
- Admin panel → **Workers → 🐙 GitHub Actions Workspaces** आणि Telegram `/cmd → ☁️ Colab Worker` मध्ये नवीन buttons.
- अनेक GitHub accounts चे token + repo (`owner/repo`) add करता येतात. प्रत्येक workspace ला ✔ (Use) देऊन निवडायचं.
- Watermark queue मध्ये job आला की निवडलेले **सगळे** workspaces `repository_dispatch` ने start होतात.
- Jobs संपले की **Idle kill** (default 1 min, 0 = लगेच) नंतर सगळे runs kill.
- **Parallel workspaces** (default 2): एकाच वेळी इतके workspaces (वेगळे accounts/repos) चालतात; प्रत्येकाला स्वतःचा GitHub runner (एका workspace वर एकच run). आजचा वापर कमी असलेला workspace आधी start होतो.
- **Workspace daily timer:** प्रत्येक workspace ला वेगळा daily timer — Telegram `⏱ Workspace Daily Timer` किंवा panel मधला `⏱ timer बदला`. `0` = default. "Default Daily Hours" सगळ्यांसाठी.
- **Error protection:** GitHub/account error (401/403/404/422) किंवा workflow सलग 2 वेळा लगेच fail → त्या workspace वर run सुरू करणं बंद (⛔ blocked). ठीक केल्यावर ▶️ Start (किंवा Use परत ✔) दिल्यावर clear. 5xx/rate-limit → 10 min थांबतो.
- एक run कमाल **Max run** (default 5h) → cancel → **Restart wait** (default 20 min, बदलता येतो) → काम बाकी असेल तर परत start.
- प्रत्येक workspace ला दिवसाला कमाल **Daily limit** (default 10h, IST दिवस).
- दोन dispatch मध्ये किमान 3 min अंतर (spam/loop नाही).
- Token DB मध्ये AES-GCM encrypted (key = `GH_TOKEN_KEY` env, नसेल तर `COLAB_SECRET_KEY`+`BOT_TOKEN` वरून).

## Telegram buttons (/cmd → ☁️ Colab Worker)
GitHub Status • Auto ON/OFF • Add Workspace • Select Workspaces • Start • Stop • Parallel Runs • Idle Kill • Restart Wait (min) • Max Run Hours • Daily Hours Limit • Remove Workspace

## प्रत्येक repo मध्ये workflow  (`.github/workflows/worker.yml`)
```yaml
name: watermark-worker
on:
  repository_dispatch:
    types: [start_worker]      # admin panel मधला "event type" हाच ठेवा
  workflow_dispatch:
# टीप: concurrency group वापरू नका — parallel runs साठी प्रत्येक run स्वतंत्र हवा.
jobs:
  worker:
    runs-on: ubuntu-latest
    timeout-minutes: 350
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with: { python-version: "3.11" }
      - run: sudo apt-get update && sudo apt-get install -y ffmpeg
      - run: pip install -r requirements.txt
      - run: python worker.py      # तुमची Colab worker script (SERVER_URL + COLAB_SECRET_KEY secrets वापरून /api/colab/poll)
        env:
          SERVER_URL: ${{ secrets.SERVER_URL }}
          COLAB_SECRET_KEY: ${{ secrets.COLAB_SECRET_KEY }}
```

## Token permissions
- Fine-grained PAT: *Actions: Read & write* + *Contents: Read & write* (त्या repo साठी).
- Classic PAT: `repo` + `workflow`.

## टीप
- Run बंद केल्यावर त्या worker वरचे चालू jobs `StartColabWatcher` आपोआप परत queue मध्ये टाकतो.
- Multiple server replicas असतील तरी orchestrator एकाच ठिकाणी चालतो (DB lease).
