// A program that embeds say through libsay: it opens a node, signs in its
// user, posts to a room and reads the event back from a session.
//
//   make libsay
//   cc examples/c/notes.c -Ibuild -Lbuild -lsay -Wl,-rpath,'$ORIGIN' -o build/notes
//   build/notes /tmp/notes.db
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "libsay.h"

static void die(const char *what, char *err) {
	fprintf(stderr, "%s: %s\n", what, err ? err : "failed");
	say_free(err);
	exit(1);
}

int main(int argc, char **argv) {
	char *err = NULL;
	int64_t node = say_open(argc > 1 ? argv[1] : "notes.db", &err);
	if (!node) die("open", err);

	char *user = say_local_user(node, "me", "Me", "notes-c", &err);
	if (!user) die("local user", err);
	// The token is the value of "token" in the JSON.
	char token[128] = {0};
	sscanf(strstr(user, "\"token\":\"") + 9, "%127[^\"]", token);
	say_free(user);

	int status = 0;
	char *room = say_request(node, "POST", "/api/v1/rooms", token, "{\"kind\":\"group\",\"name\":\"Notes\"}", &status, &err);
	if (!room) die("create room", err);
	char room_id[64] = {0};
	sscanf(strstr(room, "\"id\":\"") + 6, "%63[^\"]", room_id);
	say_free(room);
	printf("room %s (%d)\n", room_id, status);

	int64_t conn = say_connect(node, token, &err);
	if (!conn) die("connect", err);
	char env[512];
	snprintf(env, sizeof env, "{\"id\":\"1\",\"type\":\"subscribe\",\"payload\":{\"room_id\":\"%s\",\"after\":0}}", room_id);
	if (say_send(conn, env, &err) != 0) die("subscribe", err);
	snprintf(env, sizeof env, "{\"id\":\"2\",\"type\":\"send\",\"payload\":{\"room_id\":\"%s\",\"parts\":[{\"kind\":\"text\",\"data\":{\"text\":\"written in C\"}}]}}", room_id);
	if (say_send(conn, env, &err) != 0) die("send", err);

	for (;;) {
		char *msg = say_recv(conn, 5000, &err);
		if (!msg) die("recv", err);
		int done = strstr(msg, "\"type\":\"event\"") != NULL;
		if (done) printf("event %s\n", msg);
		say_free(msg);
		if (done) break;
	}
	say_disconnect(conn);
	return say_close(node) == 0 ? 0 : 1;
}
