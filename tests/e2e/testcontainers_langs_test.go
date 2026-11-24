//go:build e2e && darwin

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Testcontainers for Python, Java, Node.js (under Node and Bun) and Rust
// drive the engine as their users run them: a project of each in a
// directory of its own, at the newest release, starts fws/postgres:16 and
// fws/redis:8 through the library's modules with the image names
// overridden, queries both, and removes what it started. Each reads
// DOCKER_HOST, which TestMain sets to the engine's socket. Their reapers
// run from testcontainers/ryuk at the version each pins, which the
// catalog's fws/ryuk:0.14.0 is tagged as (README, Testcontainers).

// langTools looks up the programs a test runs, and skips the test when
// the runner lacks one.
func langTools(t *testing.T, names ...string) map[string]string {
	t.Helper()
	paths := map[string]string{}
	for _, n := range names {
		p, err := exec.LookPath(n)
		if err != nil {
			t.Skipf("%s is not installed on this machine: %v", n, err)
		}
		paths[n] = p
	}
	return paths
}

// writeTree writes files, by their paths relative to dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// langRun runs a toolchain's command in dir, with env beside the suite's
// own, and returns its output; a failure ends the test with its tail.
func langRun(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	// Toolchains fetch their packages on a first run.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, tail(string(out), 120))
	}
	return string(out)
}

// expectRemoved waits for the containers carrying label to be gone,
// but the reaper's, which outlives its client by its reconnection
// timeout, and fails the test with any that stay.
func expectRemoved(t *testing.T, label string) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) {
		out := mustDocker(t, "ps", "-a", "--no-trunc", "--filter", "label="+label, "--format", "{{.ID}} {{.Image}} {{.Status}}")
		var left []string
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l != "" && !strings.Contains(l, "testcontainers/ryuk") {
				left = append(left, l)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("containers labelled %s stayed:\n%s", label, strings.Join(left, "\n"))
			return
		}
	}
}

const pythonTest = `import psycopg

from testcontainers.community.postgres import PostgresContainer
from testcontainers.community.redis import RedisContainer
from testcontainers.core.container import DockerContainer
from testcontainers.core.network import Network


def test_postgres():
    with PostgresContainer("fws/postgres:16", driver=None) as pg:
        with psycopg.connect(pg.get_connection_url()) as conn:
            assert conn.execute("SELECT 1 + 1").fetchone()[0] == 2


def test_redis_on_a_network():
    with Network() as net:
        with RedisContainer("fws/redis:8").with_network(net).with_network_aliases("cache") as redis:
            client = redis.get_client()
            client.set("greeting", "hello")
            assert client.get("greeting") == b"hello"
            with DockerContainer("fws/toolbox:1").with_network(net).with_command(["dial", "cache:6379"]) as probe:
                assert probe.wait() == 0
`

// Testcontainers for Python runs its modules' Postgres and Redis, which
// wait by running psql in the container and by pinging Redis, a
// container on a network of its own that reaches another by its alias,
// and its reaper from the Ryuk version it pins.
func TestTestcontainersPython(t *testing.T) {
	tools := langTools(t, "python3")
	dir := t.TempDir()
	langRun(t, dir, nil, tools["python3"], "-m", "venv", "venv")
	bin := filepath.Join(dir, "venv", "bin")
	langRun(t, dir, nil, filepath.Join(bin, "pip"), "install", "--disable-pip-version-check", "-q",
		"testcontainers==4.15.0", "redis==8.2.0", "psycopg[binary]==3.3.9", "pytest==9.2.0")
	// testcontainers-python 4.15.0 names Ryuk 0.8.1 (its core/config.py);
	// the catalog's 0.14.0 speaks the same protocol.
	mustDocker(t, "tag", "fws/ryuk:0.14.0", "testcontainers/ryuk:0.8.1")
	writeTree(t, dir, map[string]string{"test_fws.py": pythonTest})
	out := langRun(t, dir, nil, filepath.Join(bin, "pytest"), "-q", "-p", "no:cacheprovider", "test_fws.py")
	if !strings.Contains(out, "2 passed") {
		t.Errorf("pytest:\n%s", tail(out, 40))
	}
	expectRemoved(t, "org.testcontainers.lang=python")
}

const javaPOM = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>fws.e2e</groupId>
  <artifactId>testcontainers-java</artifactId>
  <version>1</version>
  <properties>
    <maven.compiler.release>17</maven.compiler.release>
    <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
  </properties>
  <dependencyManagement>
    <dependencies>
      <dependency>
        <groupId>org.testcontainers</groupId>
        <artifactId>testcontainers-bom</artifactId>
        <version>2.0.5</version>
        <type>pom</type>
        <scope>import</scope>
      </dependency>
      <dependency>
        <groupId>org.junit</groupId>
        <artifactId>junit-bom</artifactId>
        <version>5.14.4</version>
        <type>pom</type>
        <scope>import</scope>
      </dependency>
    </dependencies>
  </dependencyManagement>
  <dependencies>
    <dependency>
      <groupId>org.testcontainers</groupId>
      <artifactId>testcontainers-postgresql</artifactId>
      <scope>test</scope>
    </dependency>
    <dependency>
      <groupId>org.junit.jupiter</groupId>
      <artifactId>junit-jupiter</artifactId>
      <scope>test</scope>
    </dependency>
    <dependency>
      <groupId>org.postgresql</groupId>
      <artifactId>postgresql</artifactId>
      <version>42.7.15</version>
      <scope>test</scope>
    </dependency>
    <dependency>
      <groupId>org.slf4j</groupId>
      <artifactId>slf4j-simple</artifactId>
      <version>2.0.20</version>
      <scope>test</scope>
    </dependency>
  </dependencies>
  <build>
    <plugins>
      <plugin>
        <groupId>org.apache.maven.plugins</groupId>
        <artifactId>maven-compiler-plugin</artifactId>
        <version>3.16.0</version>
      </plugin>
      <plugin>
        <groupId>org.apache.maven.plugins</groupId>
        <artifactId>maven-surefire-plugin</artifactId>
        <version>3.6.1</version>
      </plugin>
    </plugins>
  </build>
</project>
`

const javaTest = `package fws;

import static org.junit.jupiter.api.Assertions.assertEquals;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.ResultSet;
import org.junit.jupiter.api.Test;
import org.testcontainers.containers.GenericContainer;
import org.testcontainers.containers.Network;
import org.testcontainers.containers.startupcheck.OneShotStartupCheckStrategy;
import org.testcontainers.containers.wait.strategy.Wait;
import org.testcontainers.postgresql.PostgreSQLContainer;
import org.testcontainers.utility.DockerImageName;

class FwsTest {
    static final DockerImageName POSTGRES = DockerImageName.parse("fws/postgres:16").asCompatibleSubstituteFor("postgres");

    @Test
    void postgres() throws Exception {
        try (PostgreSQLContainer pg = new PostgreSQLContainer(POSTGRES)) {
            pg.start();
            try (Connection c = DriverManager.getConnection(pg.getJdbcUrl(), pg.getUsername(), pg.getPassword());
                 ResultSet rs = c.createStatement().executeQuery("SHOW fsync")) {
                rs.next();
                // The module's command, postgres -c fsync=off, reached the server.
                assertEquals("off", rs.getString(1));
            }
        }
    }

    @Test
    void redisOnANetwork() throws Exception {
        try (Network net = Network.newNetwork();
             GenericContainer<?> redis = new GenericContainer<>(DockerImageName.parse("fws/redis:8"))
                 .withExposedPorts(6379)
                 .withNetwork(net)
                 .withNetworkAliases("cache")
                 .waitingFor(Wait.forLogMessage(".*Ready to accept connections.*\\n", 1))) {
            redis.start();
            try (Socket s = new Socket(redis.getHost(), redis.getMappedPort(6379))) {
                s.getOutputStream().write("PING\r\n".getBytes(StandardCharsets.US_ASCII));
                BufferedReader in = new BufferedReader(new InputStreamReader(s.getInputStream(), StandardCharsets.US_ASCII));
                assertEquals("+PONG", in.readLine());
            }
            try (GenericContainer<?> probe = new GenericContainer<>(DockerImageName.parse("fws/toolbox:1"))
                     .withNetwork(net)
                     .withCommand("dial", "cache:6379")
                     .withStartupCheckStrategy(new OneShotStartupCheckStrategy())) {
                probe.start();
            }
        }
    }
}
`

// Testcontainers for Java runs its PostgreSQL module with fws/postgres as
// a compatible substitute for postgres, a GenericContainer on a Network
// reached by its alias, and its reaper, Ryuk 0.14.0, which it runs
// privileged by default. Its startup checks are off, as the README tells
// its users: the file-mounting check runs alpine:3.17, a Linux image.
func TestTestcontainersJava(t *testing.T) {
	tools := langTools(t, "java", "mvn")
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"pom.xml":                        javaPOM,
		"src/test/java/fws/FwsTest.java": javaTest,
	})
	langRun(t, dir, []string{"TESTCONTAINERS_CHECKS_DISABLE=true"}, tools["mvn"], "-q", "-B", "test")
	expectRemoved(t, "org.testcontainers.lang=java")
}

const nodePackage = `{
  "name": "fws-testcontainers-node",
  "private": true,
  "type": "module",
  "dependencies": {
    "@testcontainers/postgresql": "12.2.2",
    "@testcontainers/redis": "12.2.2",
    "pg": "8.23.1",
    "testcontainers": "12.2.2"
  }
}
`

const nodeScript = `import assert from "node:assert/strict";
import net from "node:net";
import pg from "pg";
import { GenericContainer, Network, Wait } from "testcontainers";
import { PostgreSqlContainer } from "@testcontainers/postgresql";
import { RedisContainer } from "@testcontainers/redis";

// One RESP command over a fresh connection, and the server's first line.
function redis(host, port, ...args) {
  return new Promise((resolve, reject) => {
    const s = net.connect(port, host);
    let got = "";
    s.on("data", (b) => {
      got += b.toString();
      if (got.includes("\r\n")) {
        s.end();
        resolve(got.split("\r\n")[0]);
      }
    });
    s.on("error", reject);
    s.write("*" + args.length + "\r\n" + args.map((a) => "$" + a.length + "\r\n" + a + "\r\n").join(""));
  });
}

const postgres = await new PostgreSqlContainer("fws/postgres:16").start();
try {
  const client = new pg.Client({ connectionString: postgres.getConnectionUri() });
  await client.connect();
  const { rows } = await client.query("SELECT 1 + 1 AS two");
  assert.equal(rows[0].two, 2);
  await client.end();
} finally {
  await postgres.stop();
}

const network = await new Network().start();
try {
  const cache = await new RedisContainer("fws/redis:8").withNetwork(network).withNetworkAliases("cache").start();
  try {
    const host = cache.getHost(), port = cache.getMappedPort(6379);
    assert.equal(await redis(host, port, "SET", "greeting", "hello"), "+OK");
    assert.equal(await redis(host, port, "GET", "greeting"), "$5");
    const probe = await new GenericContainer("fws/toolbox:1")
      .withNetwork(network)
      .withCommand(["dial", "cache:6379"])
      .withWaitStrategy(Wait.forOneShotStartup())
      .start();
    await probe.stop();
  } finally {
    await cache.stop();
  }
} finally {
  await network.stop();
}
console.log("fws: testcontainers-node passed");
`

// Testcontainers for Node.js, under Node and under Bun: its PostgreSQL
// module, which waits for the image's health check and the listening
// port, its Redis module on a network, reached by its alias, and its
// reaper, Ryuk 0.14.0, as testcontainers 12.2.2 pins.
func TestTestcontainersNode(t *testing.T) {
	t.Run("node", func(t *testing.T) {
		tools := langTools(t, "node", "npm")
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"package.json": nodePackage, "fws.mjs": nodeScript})
		langRun(t, dir, nil, tools["npm"], "install", "--no-audit", "--no-fund", "--loglevel=error")
		if out := langRun(t, dir, nil, tools["node"], "fws.mjs"); !strings.Contains(out, "fws: testcontainers-node passed") {
			t.Errorf("node fws.mjs:\n%s", tail(out, 40))
		}
		expectRemoved(t, "org.testcontainers.lang=node")
	})
	t.Run("bun", func(t *testing.T) {
		tools := langTools(t, "bun")
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"package.json": nodePackage, "fws.mjs": nodeScript})
		langRun(t, dir, nil, tools["bun"], "install", "--no-save")
		if out := langRun(t, dir, nil, tools["bun"], "fws.mjs"); !strings.Contains(out, "fws: testcontainers-node passed") {
			t.Errorf("bun fws.mjs:\n%s", tail(out, 40))
		}
		expectRemoved(t, "org.testcontainers.lang=node")
	})
}

// testcontainers-modules 0.15.0, the newest, builds on testcontainers
// 0.27, whose newest is 0.27.3.
const rustCargo = `[package]
name = "fws-testcontainers-rs"
version = "0.1.0"
edition = "2021"
publish = false

[dev-dependencies]
testcontainers = { version = "=0.27.3", features = ["blocking"] }
testcontainers-modules = { version = "=0.15.0", features = ["postgres", "redis", "blocking"] }
postgres = "=0.19.14"
`

const rustTest = `use std::io::{BufRead, BufReader, Write};
use std::net::TcpStream;

use testcontainers::core::WaitFor;
use testcontainers::core::wait::ExitWaitStrategy;
use testcontainers::runners::SyncRunner;
use testcontainers::{GenericImage, ImageExt};
use testcontainers_modules::{postgres::Postgres, redis::Redis};

#[test]
fn postgres() {
    let node = Postgres::default().with_name("fws/postgres").with_tag("16").start().unwrap();
    let url = format!(
        "host={} port={} user=postgres password=postgres dbname=postgres",
        node.get_host().unwrap(),
        node.get_host_port_ipv4(5432).unwrap()
    );
    let mut client = postgres::Client::connect(&url, postgres::NoTls).unwrap();
    let two: i32 = client.query_one("SELECT 1 + 1", &[]).unwrap().get(0);
    assert_eq!(two, 2);
}

#[test]
fn redis_on_a_network() {
    let cache = Redis::default()
        .with_name("fws/redis")
        .with_tag("8")
        .with_network("fws-e2e-rs")
        .with_container_name("fws-e2e-rs-cache")
        .start()
        .unwrap();
    let mut s = TcpStream::connect((cache.get_host().unwrap().to_string(), cache.get_host_port_ipv4(6379).unwrap())).unwrap();
    s.write_all(b"PING\r\n").unwrap();
    let mut line = String::new();
    BufReader::new(&s).read_line(&mut line).unwrap();
    assert_eq!(line.trim_end(), "+PONG");
    let probe = GenericImage::new("fws/toolbox", "1")
        .with_wait_for(WaitFor::exit(ExitWaitStrategy::new().with_exit_code(0)))
        .with_network("fws-e2e-rs")
        .with_cmd(["dial", "fws-e2e-rs-cache:6379"])
        .start()
        .unwrap();
    drop(probe);
}
`

// testcontainers-rs and its community modules: Postgres and Redis with
// their image names overridden, a network whose members reach one
// another by container name, and containers removed when dropped. It
// runs no reaper.
func TestTestcontainersRust(t *testing.T) {
	tools := langTools(t, "cargo")
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"Cargo.toml": rustCargo, "src/lib.rs": "", "tests/fws.rs": rustTest})
	langRun(t, dir, nil, tools["cargo"], "test", "--quiet", "--", "--test-threads=1")
	expectRemoved(t, "org.testcontainers.managed-by=testcontainers")
}
