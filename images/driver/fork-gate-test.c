// Self-test for pagewire's fork gate (images/driver/patches/pappl-retrofit-fork-gate.patch).
// Built twice in images/driver/Dockerfile: linked to libpappl-retrofit (gated) and without it.
//
// Usage: fork-gate-test <case>; the child of fork() takes the lock with a blocking call.
//   wake      a thread holds a mutex and lets it go when the fork starts waiting
//   wake-rw   the same with a rwlock held for writing
//   held      a thread holds a mutex until fork() has returned in the parent
//   held-rw   the same with a rwlock held for writing
//   condwait  a thread sleeps in pthread_cond_wait() during fork(); the child broadcasts
//   owncond   the same while the forking thread holds that thread's mutex
//   cancel    a thread was cancelled in pthread_cond_wait() before fork()
//   tz        a thread calls localtime_r() in a loop during 100 forks
//   timegm    the same with timegm(), which glibc runs through its own gmtime_r()
//   syslog    no fork: syslog() through the gate prints "fork-gate-test: syslog 42" on stderr
// Exit status: 0 the child took the lock, 2 a child hung (alarm), 3 a child exited 70
// (the gate's "would wait forever" exit), 1 anything else.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <linux/futex.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdatomic.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <syslog.h>
#include <time.h>
#include <unistd.h>

static pthread_mutex_t m = PTHREAD_MUTEX_INITIALIZER, cm = PTHREAD_MUTEX_INITIALIZER;
static pthread_rwlock_t rw = PTHREAD_RWLOCK_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;
static int ready_p[2], go_p[2], rwmode, posted;
static atomic_int stop;
static const char *mode;

static void put(int fd) { char c = 0; if (write(fd, &c, 1) != 1) _exit(1); }
static void get(int fd) { char c; if (read(fd, &c, 1) != 1) _exit(1); }
static int is(const char *s) { return (!strcmp(mode, s)); }

// The gate sleeps in FUTEX_WAIT_BITSET while a lock is held: in the wake cases
// that is when the holder lets go, so only the gate's own wake-up lets fork() go on.
long syscall(long n, ...)
{
  static long (*real)(long, ...);
  long a[6];
  va_list ap;

  va_start(ap, n);
  for (int i = 0; i < 6; i ++)
    a[i] = va_arg(ap, long);
  va_end(ap);
  if (!real)
    real = (long (*)(long, ...))dlsym(RTLD_NEXT, "syscall");
  if (n == SYS_futex && (a[1] & FUTEX_CMD_MASK) == FUTEX_WAIT_BITSET && !strncmp(mode, "wake", 4) && !posted)
  {
    posted = 1;
    put(go_p[1]);
  }
  return (real(n, a[0], a[1], a[2], a[3], a[4], a[5]));
}

static void *holder(void *d)
{
  (void)d;
  if (rwmode) pthread_rwlock_wrlock(&rw); else pthread_mutex_lock(&m);
  put(ready_p[1]);
  get(go_p[0]);
  if (rwmode) pthread_rwlock_unlock(&rw); else pthread_mutex_unlock(&m);
  return (NULL);
}

static void *waiter(void *d)
{
  (void)d;
  pthread_mutex_lock(&cm);
  put(ready_p[1]);
  while (!stop) pthread_cond_wait(&cv, &cm);
  pthread_mutex_unlock(&cm);
  return (NULL);
}

static void unlock_cm(void *d)
{
  (void)d;
  pthread_mutex_unlock(&cm);
}

static void *cancelled(void *d)
{
  (void)d;
  pthread_mutex_lock(&cm);
  pthread_cleanup_push(unlock_cm, NULL);
  put(ready_p[1]);
  for (;;) pthread_cond_wait(&cv, &cm);
  pthread_cleanup_pop(1);
  return (NULL);
}

static void *spin(void *d)
{
  struct tm tm = {.tm_mday = 1, .tm_year = 70};
  int gm = is("timegm");
  (void)d;
  for (time_t t = 0; !stop; t ++)
    if (gm) timegm(&tm); else localtime_r(&t, &tm);
  return (NULL);
}

// Fork once; the child takes the case's lock and exits 0.
static int fork_child(void)
{
  pid_t pid;
  int status;

  if ((pid = fork()) == 0)
  {
    struct tm tm;
    time_t t = 0;
    alarm(3);
    if (rwmode) pthread_rwlock_wrlock(&rw);
    else if (is("tz") || is("timegm")) localtime_r(&t, &tm);
    else if (is("owncond")) { pthread_mutex_unlock(&cm); pthread_mutex_lock(&cm); }
    else if (is("condwait")) { pthread_mutex_lock(&cm); pthread_cond_broadcast(&cv); }	// must not wait for the parent's sleeper
    else if (is("cancel")) pthread_mutex_lock(&cm);
    else pthread_mutex_lock(&m);
    _exit(0);
  }
  if (pid < 0 || waitpid(pid, &status, 0) != pid) return (1);
  if (WIFSIGNALED(status)) return (2);
  if (WEXITSTATUS(status) == 70) return (3);
  return (WEXITSTATUS(status) ? 1 : 0);
}

static void on_parent(void) { if (!strncmp(mode, "held", 4)) put(go_p[1]); }

int main(int argc, char **argv)
{
  pthread_t t;
  int ret = 0;

  if (argc != 2) return (1);
  mode = argv[1];
  rwmode = strstr(mode, "-rw") != NULL;
  if (pipe(ready_p) || pipe(go_p)) return (1);
  pthread_atfork(NULL, on_parent, NULL);

  if (is("tz") || is("timegm"))
  {
    pthread_create(&t, NULL, spin, NULL);
    for (int i = 0; i < 100 && !ret; i ++) ret = fork_child();
    stop = 1;
    pthread_join(t, NULL);
  }
  else if (is("condwait") || is("owncond"))
  {
    pthread_create(&t, NULL, waiter, NULL);
    get(ready_p[0]);
    pthread_mutex_lock(&cm);		// returns once the waiter sleeps in pthread_cond_wait()
    if (is("condwait")) pthread_mutex_unlock(&cm);
    ret = fork_child();
    if (is("condwait")) pthread_mutex_lock(&cm);
    stop = 1;
    pthread_cond_broadcast(&cv);
    pthread_mutex_unlock(&cm);
    pthread_join(t, NULL);
  }
  else if (is("cancel"))
  {
    pthread_create(&t, NULL, cancelled, NULL);
    get(ready_p[0]);
    pthread_cancel(t);
    pthread_join(t, NULL);
    ret = fork_child();
  }
  else if (is("wake") || is("wake-rw") || is("held") || is("held-rw"))
  {
    pthread_create(&t, NULL, holder, NULL);
    get(ready_p[0]);
    ret = fork_child();
    if (!strncmp(mode, "wake", 4) && !posted) put(go_p[1]);	// no gate: let the holder finish
    pthread_join(t, NULL);
  }
  else if (is("syslog"))
  {
    openlog("fork-gate-test", LOG_PERROR, LOG_USER);
    syslog(LOG_ERR, "%s %d", mode, 42);	// __syslog_chk() with -O2 and _FORTIFY_SOURCE, as the libraries call it
    closelog();
  }
  else
    return (1);
  printf("%s: %d\n", mode, ret);
  return (ret);
}
