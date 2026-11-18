// hog touches 512 MiB, 16 MiB at a time, and prints "survived" if nothing
// stopped it. With "child" it runs itself as a hog and prints how that
// child ended; with "hold" it waits to be killed.
package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hold" {
		select {}
	}
	if len(os.Args) > 1 && os.Args[1] == "child" {
		err := exec.Command(os.Args[0]).Run()
		fmt.Println("child:", err)
		return
	}
	var keep [][]byte
	for range 32 {
		b := make([]byte, 16<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		keep = append(keep, b)
	}
	fmt.Println("survived", len(keep))
}
