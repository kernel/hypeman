/*
 * Guest init for the jailed snapshot test. It prints a counter once a second,
 * so a restored guest shows whether its memory (and counter) survived.
 *
 * Build statically:  gcc -static -O2 -o init heartbeat-init.c
 */
#include <stdio.h>
#include <unistd.h>

int main(void)
{
	unsigned long n = 0;
	for (;;) {
		printf("HB %lu\n", n++);
		fflush(stdout);
		sleep(1);
	}
}
