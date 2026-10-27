//go:build dev

package main

// The dev build tag loads .env for local runs (make run, make watch). Release
// builds omit the tag, so the shipped binary never reads a .env file.
import _ "github.com/joho/godotenv/autoload"
