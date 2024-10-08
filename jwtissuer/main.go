package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/golang-jwt/jwt/v5"
)

var (
	version   = "v0.0.1"
	goVersion = flag.Bool("version", false, "Displays the current server version.")
	jwtSecret []byte
)

func init() {
	flag.Parse()
	secret, provided := os.LookupEnv("DRAND_AUTH_KEY")
	if !provided {
		secret = flag.Arg(0)
	} else {
		slog.Error("Using DRAND_AUTH_KEY var env, ignoring binary arguments")
	}
	if len(secret) < 256 {
		log.Fatal("DRAND_AUTH_KEY not provided as a 128 byte hex-encoded secret in argument. Got ", len(secret), " char: ", secret)
	} else {
		var err error
		jwtSecret, err = hex.DecodeString(secret)
		if err != nil {
			log.Fatal("unable to parse AUTH_TOKEN as valid hex")
		}
	}
}

func main() {
	if *goVersion {
		log.Fatal("drand http JWT issuer version: ", version)
	}

	// Create a new token object
	token := jwt.New(jwt.SigningMethodHS256)

	// Create a JWT and send it as response
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		log.Fatal("Error while signing the token")
	}

	response := map[string]string{
		"token": tokenString,
	}

	log.Println("Created a valid JWT", "token", tokenString)
	res, err := json.Marshal(response)
	if err != nil {
		log.Fatal("Unable to marshal JWT token:", err)
	}

	fmt.Println(string(res))
}
