// Self-test for images/driver/patches/pappl-retrofit-filter-reap.patch.
// Built twice in images/driver/Dockerfile: with RUN_CHAIN=_prFilterChain, the patched
// libpappl-retrofit function, and with RUN_CHAIN=cfFilterChain, as pappl-retrofit 1.0b3's
// _prFilter() called the chain from the job thread.
//
// Usage: filter-chain-test <case>
//   steal  another thread reaps the chain's filter while the chain starts it (another
//          job's wait()); the chain must still end
//   reap   a child the thread started before the chain has exited; the chain must
//          leave it to its owner
//   both   two threads run a chain at once, each filter waiting for the other's
//   orphan the chain's filter leaves a process behind; the chain must reap it before it
//          returns, not leave it to the caller (the printer application is PID 1)
//   pipefail the chain cannot open the pipe after its first filter (EMFILE) while that
//          filter writes more than a pipe holds; the chain must still end
// Exit status: 0 the case passed, 2 a chain hung (alarm), 3 a child was reaped by the
// wrong waiter or left to the caller, 1 anything else.
#define _GNU_SOURCE
#include <cupsfilters/filter.h>
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/wait.h>
#include <unistd.h>

extern int _prFilterChain(int inputfd, int outputfd, int inputseekable, cf_filter_data_t *data, cups_array_t *chain);

static int started_p[2], go_p[2], release_p[2], ab_p[2], ba_p[2];
static int pipefail;			// the pipefail case runs: three filters, one error expected

static void put(int fd, int v) { if (write(fd, &v, sizeof(v)) != sizeof(v)) _exit(1); }
static int get(int fd) { int v; if (read(fd, &v, sizeof(v)) != sizeof(v)) _exit(1); return (v); }
static void on_alarm(int sig) { (void)sig; _exit(2); }

// cfFilterChain() logs "<name> (PID <pid>) started." right after it forks a filter and
// before it waits: the steal case holds the chain there, so the test reaps first.
static void logfunc(void *d, cf_loglevel_t level, const char *message, ...)
{
  char buf[1024];
  int pid, n = 0;
  va_list ap;

  va_start(ap, message);
  vsnprintf(buf, sizeof(buf), message, ap);
  va_end(ap);
  if (d && sscanf(buf, "cfFilterChain: hold (PID %d) started.%n", &pid, &n) == 1 && n > 0)
  {
    put(started_p[1], pid);
    get(go_p[0]);
  }
  if (level >= CF_LOGLEVEL_ERROR && !(pipefail && strstr(buf, "Could not create pipe")))
    fprintf(stderr, "%s\n", buf);
}

static int hold(int in, int out, int seekable, cf_filter_data_t *data, void *p)
{
  (void)in; (void)out; (void)seekable; (void)data; (void)p;
  get(release_p[0]);
  return (0);
}

static int quick(int in, int out, int seekable, cf_filter_data_t *data, void *p)
{
  (void)in; (void)out; (void)seekable; (void)data; (void)p;
  return (0);
}

// The two filters of the both case: each answers the other, so both chains wait at once.
static int ping(int in, int out, int seekable, cf_filter_data_t *data, void *p)
{
  (void)in; (void)out; (void)seekable; (void)data; (void)p;
  put(ab_p[1], 1);
  return (get(ba_p[0]) == 2 ? 0 : 1);
}

static int pong(int in, int out, int seekable, cf_filter_data_t *data, void *p)
{
  (void)in; (void)out; (void)seekable; (void)data; (void)p;
  put(ba_p[1], get(ab_p[0]) + 1);
  return (0);
}

// Starts a process that outlives the filter, as the "Backend" filter's CUPS backend does.
static int leave(int in, int out, int seekable, cf_filter_data_t *data, void *p)
{
  pid_t pid;

  (void)in; (void)out; (void)seekable; (void)data; (void)p;
  if ((pid = fork()) == 0)
  {
    get(release_p[0]);
    _exit(0);
  }
  put(started_p[1], pid);
  return (pid < 0);
}

// The first filter of the pipefail case: writes until its output breaks.
static int flood(int in, int out, int seekable, cf_filter_data_t *data, void *p)
{
  static char buf[4096];

  (void)in; (void)seekable; (void)data; (void)p;
  put(started_p[1], 0);
  while (write(out, buf, sizeof(buf)) > 0);
  return (0);
}

struct chain { cf_filter_function_t f; char *name; int hook; int ret; };

static void *run(void *arg)
{
  struct chain *c = arg;
  cf_filter_filter_in_chain_t filter[3] = { { c->f, NULL, c->name }, { c->f, NULL, c->name }, { c->f, NULL, c->name } };
  cf_filter_data_t data;
  cups_array_t *chain = cupsArrayNew(NULL, NULL);

  memset(&data, 0, sizeof(data));
  data.back_pipe[0] = data.back_pipe[1] = data.side_pipe[0] = data.side_pipe[1] = -1;
  data.logfunc = logfunc;
  data.logdata = c->hook ? c : NULL;
  for (int i = 0; i < (pipefail ? 3 : 1); i++)
    cupsArrayAdd(chain, &filter[i]);
  c->ret = RUN_CHAIN(open("/dev/null", O_RDONLY), open("/dev/null", O_WRONLY), 0, &data, chain);
  cupsArrayDelete(chain);
  return (NULL);
}

int main(int argc, char **argv)
{
  pthread_t t, u;
  pid_t pid;
  siginfo_t si;
  int status;

  if (argc != 2 || pipe(started_p) || pipe(go_p) || pipe(release_p) || pipe(ab_p) || pipe(ba_p))
    return (1);
  signal(SIGALRM, on_alarm);
  alarm(3);				// the only deadline: a chain that never ends

  if (!strcmp(argv[1], "steal"))
  {
    struct chain a = { hold, "hold", 1, -1 };

    pthread_create(&t, NULL, run, &a);
    pid = get(started_p[0]);
    put(release_p[1], 0);
    // The chain is held before its wait(): only this waitpid() can reap the filter.
    if (waitpid(pid, &status, 0) == pid)
      fprintf(stderr, "steal: reaped the chain's filter %d from another thread\n", (int)pid);
    put(go_p[1], 0);
    pthread_join(t, NULL);
    return (a.ret ? 1 : 0);
  }
  else if (!strcmp(argv[1], "reap"))
  {
    struct chain a = { quick, "quick", 0, -1 };

    if ((pid = fork()) == 0)
      _exit(0);
    // Exited, not reaped: a wait() for any child returns it first (it is older).
    if (pid < 0 || waitid(P_PID, (id_t)pid, &si, WEXITED | WNOWAIT))
      return (1);
    run(&a);
    if (a.ret)
      return (1);
    if (waitpid(pid, &status, 0) != pid)
    {
      fprintf(stderr, "reap: the chain reaped child %d of its caller (%s)\n", (int)pid, strerror(errno));
      return (3);
    }
    return (0);
  }
  else if (!strcmp(argv[1], "both"))
  {
    struct chain a = { ping, "ping", 0, -1 }, b = { pong, "pong", 0, -1 };

    pthread_create(&t, NULL, run, &a);
    pthread_create(&u, NULL, run, &b);
    pthread_join(t, NULL);
    pthread_join(u, NULL);
    return (a.ret || b.ret ? 1 : 0);
  }
  else if (!strcmp(argv[1], "orphan"))
  {
    struct chain a = { leave, "leave", 0, -1 };

    // Orphans of the chain come to this process, as they come to PID 1 in the container.
    if (prctl(PR_SET_CHILD_SUBREAPER, 1))
      return (1);
    pthread_create(&t, NULL, run, &a);
    pid = get(started_p[0]);
    put(release_p[1], 0);
    pthread_join(t, NULL);
    if (a.ret)
      return (1);
    if (waitid(P_PID, (id_t)pid, &si, WEXITED) == 0)
    {
      fprintf(stderr, "orphan: the chain left process %d to its caller\n", (int)pid);
      return (3);
    }
    return (0);
  }
  else if (!strcmp(argv[1], "pipefail"))
  {
    struct chain a = { flood, "flood", 0, -1 };
    struct rlimit rl;
    int fd = open("/dev/null", O_RDONLY);

    // Four descriptors left: run() opens two, the pipe after the first filter takes the
    // last two, and the one after the second fails, as only the chain's input was closed.
    if (fd < 0 || close(fd) || getrlimit(RLIMIT_NOFILE, &rl))
      return (1);
    rl.rlim_cur = (rlim_t)fd + 4;
    if (setrlimit(RLIMIT_NOFILE, &rl))
      return (1);
    pipefail = 1;
    pthread_create(&t, NULL, run, &a);
    get(started_p[0]);			// the first filter runs, so the second pipe() failed
    pthread_join(t, NULL);
    return (a.ret == 1 ? 0 : 1);
  }
  return (1);
}
