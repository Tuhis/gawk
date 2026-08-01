/*
 * The C half of the PipeWire helper (R35, docs/39 D4).
 *
 * It is deliberately thin: a thread loop, a registry listener, and three
 * imperative calls. Every decision — who is emitting, which ports pair with
 * which, what to link and when — is made in Go (internal/pwgraph), because
 * that is the part that has to be unit-tested against a game launch's event
 * storm and the part that would otherwise be untestable C.
 *
 * Threading: libpipewire's thread loop runs the event loop on its own thread.
 * Callbacks fire there and do nothing but hand the event to Go, which enqueues
 * it and returns immediately — a callback that blocked would stall the whole
 * sound server's client connection. Every call *into* pipewire from Go takes
 * the loop lock first (gawk_pw_lock/unlock).
 */
#ifndef GAWK_PW_SHIM_H
#define GAWK_PW_SHIM_H

#include <pipewire/pipewire.h>
#include <stdint.h>

struct gawk_pw {
	struct pw_thread_loop *loop;
	struct pw_context *context;
	struct pw_core *core;
	struct pw_registry *registry;
	struct spa_hook registry_listener;
	struct spa_hook core_listener;
	int started;
};

/* Kinds mirrored in Go (pwgraph.Kind). */
#define GAWK_KIND_OTHER 0
#define GAWK_KIND_NODE 1
#define GAWK_KIND_PORT 2
#define GAWK_KIND_CLIENT 3

/*
 * Connects to PipeWire and starts the loop. On failure returns NULL and stores
 * a malloc'd message in *err (the caller frees it).
 */
struct gawk_pw *gawk_pw_new(char **err);

/* Stops the loop and drops the connection. Every proxy we created dies with
 * it, and — because nothing is linger-flagged — so does every object those
 * proxies own: the sink and all the links. That is the cleanup story, and it
 * holds for SIGKILL just as it does for a clean exit, because it is the
 * daemon's own reaction to a closed socket rather than anything we run. */
void gawk_pw_free(struct gawk_pw *pw);

/* Takes/releases the loop lock. Required around every call below. */
void gawk_pw_lock(struct gawk_pw *pw);
void gawk_pw_unlock(struct gawk_pw *pw);

/* Asks the daemon for a round-trip; returns the sequence number that the
 * gawkOnCoreDone callback will report back. */
int gawk_pw_sync(struct gawk_pw *pw);

/*
 * Creates the capture sink: a null audio sink with media.class
 * Audio/Sink/Internal (hidden from application device lists) and the given
 * channel layout. positions is a comma-separated SPA position list ("FL,FR").
 * Returns NULL on failure.
 */
struct pw_proxy *gawk_pw_create_sink(struct gawk_pw *pw, const char *name,
                                     const char *desc, const char *positions,
                                     int channels);

/* Creates one port-to-port link. Returns NULL on failure. */
struct pw_proxy *gawk_pw_create_link(struct gawk_pw *pw, uint32_t out_node,
                                     uint32_t out_port, uint32_t in_node,
                                     uint32_t in_port);

/* Destroys a proxy, and with it the object it created. */
void gawk_pw_destroy_proxy(struct pw_proxy *proxy);

/* spa_dict accessors, so Go can read registry properties without a second
 * copy of the struct layout. */
unsigned gawk_dict_n(const struct spa_dict *d);
const char *gawk_dict_key(const struct spa_dict *d, unsigned i);
const char *gawk_dict_value(const struct spa_dict *d, unsigned i);

/* The linked library's version, for the ready event. */
const char *gawk_pw_version(void);

#endif /* GAWK_PW_SHIM_H */
