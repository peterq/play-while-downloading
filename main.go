package main

import (
	"github.com/peterq/play-while-downloading/rpc-dl"
	"log"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	rpc_dl.Start()
}
