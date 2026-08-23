package main; import (fmt; reflect; github.com/gotd/td/tg); func main() { t := reflect.TypeOf(tg.InputReplyToMessage{}); for i := 0; i < t.NumField(); i++ { fmt.Println(t.Field(i).Name) } }
