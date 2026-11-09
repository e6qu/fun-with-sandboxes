// bench OP N DIR runs system call OP N times on files under DIR, which
// names the container's root as the process sees it, and prints the
// nanoseconds each call took. spawn starts DIR/bin/bench, this program.
#include <dirent.h>
#include <fcntl.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <stdint.h>
#include <time.h>
#include <unistd.h>

extern char **environ;

int main(int argc, char **argv) {
	if (argc != 4) {
		fprintf(stderr, "usage: bench OP N DIR\n");
		return 2;
	}
	const char *op = argv[1], *dir = argv[3];
	long n = atol(argv[2]);
	char file[1024], missing[1024], sub[1024], self[1024];
	snprintf(file, sizeof file, "%s/etc/marker", dir);
	snprintf(missing, sizeof missing, "%s/etc/missing", dir);
	snprintf(sub, sizeof sub, "%s/etc", dir);
	snprintf(self, sizeof self, "%s/bin/bench", dir);
	char buf[4096];
	struct stat st;
	int fd = open(file, O_RDONLY);
	if (fd < 0) {
		perror(file);
		return 1;
	}
	uint64_t start = clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
	for (long i = 0; i < n; i++) {
		if (!strcmp(op, "getpid")) {
			getpid();
		} else if (!strcmp(op, "getuid")) {
			getuid();
		} else if (!strcmp(op, "open")) {
			close(open(file, O_RDONLY));
		} else if (!strcmp(op, "stat")) {
			stat(file, &st);
		} else if (!strcmp(op, "stat-missing")) {
			stat(missing, &st);
		} else if (!strcmp(op, "fstat")) {
			fstat(fd, &st);
		} else if (!strcmp(op, "pread")) {
			pread(fd, buf, sizeof buf, 0);
		} else if (!strcmp(op, "readdir")) {
			DIR *d = opendir(sub);
			while (readdir(d))
				;
			closedir(d);
		} else if (!strcmp(op, "socket")) {
			close(socket(AF_INET, SOCK_STREAM, 0));
		} else if (!strcmp(op, "spawn")) {
			char *args[] = {"bench", "getpid", "1", (char *)dir, NULL};
			pid_t pid;
			if (posix_spawn(&pid, self, NULL, NULL, args, environ) != 0) {
				perror("posix_spawn");
				return 1;
			}
			waitpid(pid, NULL, 0);
		} else {
			fprintf(stderr, "no op %s\n", op);
			return 2;
		}
	}
	printf("%.1f\n", (double)(clock_gettime_nsec_np(CLOCK_UPTIME_RAW) - start) / n);
	return 0;
}
