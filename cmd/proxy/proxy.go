package main

import (
	"github.com/tum-dev/calendar-proxy/internal"
	_ "time/tzdata"
	"log"
)

func main() {
	app := &internal.App{}
	log.Println(app.Run())
}
