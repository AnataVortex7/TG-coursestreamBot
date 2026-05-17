package types

import (
	"crypto/md5"
	"encoding/hex"
	"reflect"
	"strconv"

	"github.com/gotd/td/tg"
)

type File struct {
	Location tg.InputFileLocationClass
	FileSize int64
	FileName string
	MimeType string
	ID       int64

	// Extra fields needed for worker token generation.
	// Populated by FileFromMedia — main server passes these directly to
	// NewWorkerToken so the worker never needs to re-query Telegram.
	FileType      string // "document" | "photo"
	AccessHash    int64
	FileReference []byte
	ThumbSize     string // photo thumb type (e.g. "y"); empty for documents
	DCID          int    // Telegram DC ID
}

type HashableFileStruct struct {
	FileName string
	FileSize int64
	MimeType string
	FileID   int64
}

func (f *HashableFileStruct) Pack() string {
	hasher := md5.New()
	val := reflect.ValueOf(*f)
	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)

		var fieldValue []byte
		switch field.Kind() {
		case reflect.String:
			fieldValue = []byte(field.String())
		case reflect.Int64:
			fieldValue = []byte(strconv.FormatInt(field.Int(), 10))
		}

		hasher.Write(fieldValue)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
