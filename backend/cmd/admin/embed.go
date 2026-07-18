package main

import _ "embed"

//go:embed templates/landing.html
var landingHTML string

//go:embed templates/recordings.html
var recordingsHTML string
