package main

import (
	"log"
	"fmt"

	"controllerasg/configuration"
)

const CONFIGPATH string = "../conf.json"

func main () {
	config, err := configuration.Load(CONFIGPATH)

	if err != nil {
		log.Fatalf("No se pudo cargar la configuración: %v", err)
	}

	fmt.Printf("%+v\n", config)
}