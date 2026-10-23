// Package envinit loads fsb.env as early as possible.
//
// Several packages (auth, token, email) read secrets from the process
// environment inside their init() functions, which run BEFORE main() and
// therefore before config.Load() has had a chance to read fsb.env.
// Importing this package (blank import) from those packages guarantees that
// the .env file is loaded first, because Go initialises imported packages
// before the importing package.
//
// godotenv.Load never overrides variables that are already set in the real
// environment, so Docker / Heroku / Koyeb config vars keep priority.
package envinit

import "github.com/joho/godotenv"

func init() {
	_ = godotenv.Load("fsb.env")
}
