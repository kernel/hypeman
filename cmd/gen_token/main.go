package main

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "testuser",
		"exp": time.Now().Add(24 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	signed, err := token.SignedString([]byte("hownowbrowncow"))
	if err != nil {
		panic(err)
	}
	fmt.Print(signed)
}
