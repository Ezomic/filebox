// Stands in for the streambox release binary in deploy/test/addons.sh: a real executable
// that only knows how to print the version setup.sh checks for.
package main

import "fmt"

func main() { fmt.Println("v0.1.0") }
