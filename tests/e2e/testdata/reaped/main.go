// Command reaped starts a container through Testcontainers, its reaper
// enabled, prints the container's ID and exits without removing it, which
// leaves the removal to the reaper.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func main() {
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "fws/redis:7",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(c.GetContainerID())
}
