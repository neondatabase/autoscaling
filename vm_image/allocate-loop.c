// allocate-loop.c
//
// This program allocates memory up and down in a loop, based on the user-provided amounts.
//
// USAGE:
//
//   ./allocate-loop <MIN SIZE> <MAX SIZE>
//
// Both <MIN SIZE> and <MAX SIZE> must be integers, in units of MiB. For example, running:
//
//   ./allocate-loop 128 512
//
// ... will gradually allocate from 128 MiB to 512 MiB and back down, repeating back and forth.
//
// The rate of allocation is set by internal constants, and is currently 256 MiB/s.

#define _GNU_SOURCE

#include <err.h>
#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

// Inferred stats:
// * Change by 256 MiB / second
// * Print every 0.1s (approx. every 25 MiB)
const size_t step_size = 256 * (1 << 10); // 256 KiB
const int print_every = 100;
const long step_wait_ns = 1000*1000; // Wait 1ms between steps

size_t step(size_t current, size_t previous, size_t min, size_t max);
size_t get_rss();
char* get_time();

int main(int argc, char* argv[]) {
	if (argc != 3) {
		fprintf(stderr, "bad arguments");
		return 1;
	}

	size_t min_size_mb;
	size_t max_size_mb;

	if (sscanf(argv[1], "%zu", &min_size_mb) != 1 || sscanf(argv[2], "%zu", &max_size_mb) != 1) {
		fprintf(stderr, "bad arguments: must be integers\n");
		return 1;
	}

	printf("Selected: minimum = %zu MiB, maximum = %zu MiB\n", min_size_mb, max_size_mb);

	size_t mib = 1 << 20;
	size_t min_size = min_size_mb * mib;
	size_t max_size = max_size_mb * mib;

	int memfd = memfd_create("experiment", 0);
	if (memfd == -1) {
		err(errno, "memfd");
	}

	if (ftruncate(memfd, max_size) == -1) {
		err(errno, "memfd set to min_size");
	}

	char *map = mmap(NULL, max_size, PROT_READ|PROT_WRITE, MAP_SHARED|MAP_NORESERVE, memfd, 0);
	if (map == (void *)-1) {
		err(errno, "map");
	}

	memset(map, 0xAB, min_size);
	printf("mmap created at minimum size\n");

	// Run in a loop
	size_t current_size = min_size;
	size_t previous_size = current_size;
	for (int i = 0; ; i++) {
		size_t new_size = step(current_size, previous_size, min_size, max_size);

		if (new_size > current_size) {
			// to allocate more, just write to the pages:
			memset(&map[current_size], 0xAB, new_size-current_size);
		} else if (new_size < current_size) {
			// to deallocate, use madvise() the kernel that it can reclaim them
			if (madvise(&map[new_size], current_size-new_size, MADV_REMOVE) == -1) {
				err(errno, "madvise");
			}
		} else {
			fprintf(stderr,
					"internal error: previous = %ld, current = %ld, new = %ld\n",
					previous_size, current_size, new_size);
			return 1;
		}

		if (i % print_every == 0) {
			char* time = get_time();
			size_t rss = get_rss();
			printf("%s :: %ld MiB + %ld KiB, rss = %ld\n",
					time,
					new_size / (1 << 20),
					(new_size % (1 << 20)) / (1 << 10),
					rss);
		}

		previous_size = current_size;
		current_size = new_size;

		struct timespec ts;
		ts.tv_sec = 0;
		ts.tv_nsec = step_wait_ns;
		nanosleep(&ts, NULL);
	}

	return 0;
}

size_t step(size_t current, size_t previous, size_t min, size_t max) {
	if (current == min) {
		return current + step_size;
	} else if (current == max) {
		return current - step_size;
	} else {
		return current + (current - previous);
	}
}

// note: the returned string points into a static variable. Do not use old return values across
// calls to this function.
char* get_time() {
	static char time[64] = {0};

	struct timeval now;
	gettimeofday(&now, NULL);
	int milli = now.tv_usec / 1000;

	char buf[32];
	strftime(buf, sizeof(buf), "%H:%M:%S", gmtime(&now.tv_sec));

	sprintf(time, "%s.%03d", buf, milli);
	return time;
}

size_t get_rss() {
	char* path;
	if (asprintf(&path, "/proc/%d/status", getpid()) == -1) {
		err(errno, "asprintf");
	}

	FILE* file;
	if ((file = fopen(path, "r")) == NULL) {
		err(errno, "fopen");
	}

	char* vmrss_prefix = "VmRSS:";
	int prefix_len = strlen(vmrss_prefix);

	char line_buf[256];
	while (fgets(line_buf, sizeof(line_buf), file)) {
		if (strncmp(line_buf, vmrss_prefix, prefix_len) == 0) {
			size_t rss;
			if (sscanf(line_buf + prefix_len, " %ld", &rss) != 1) {
				err(errno, "sscanf");
			}

			fclose(file);
			free(path);
			return rss;
		}
	}

	fprintf(stderr, "couldn't find VmRSS in %s\n", path);
	exit(1);
}
