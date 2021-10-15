// +build linuxacl

#ifndef PERMISSION_LINUX_H
#define PERMISSION_LINUX_H

#include <acl/libacl.h>
#include <errno.h>
#include <grp.h>
#include <pwd.h>
#include <stdio.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/acl.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

//#define DEBUG
#define USER_CHECK 0
#define GROUP_CHECK 1

struct permission_checker {
  char *user_name;
  uid_t uid;
  gid_t *gids;
  int ngids;
  char *file_path;
  struct stat file_stat;
  struct passwd pw;
};


#ifdef DEBUG
// Print out permission_checker struct.
void debug_print_checker(struct permission_checker *pc);
#endif

// Stat a given file to retrieve traditional UNIX permissions.
int stat_file(struct permission_checker *pc);

// Retrieve UID of user.
int get_user_uid(struct permission_checker *pc);

// Retrieve all groups of the user.
int get_user_groups(struct permission_checker *pc);

// Check whether user is member of a group or not.
int is_member_of_group(struct permission_checker *pc, gid_t gid);

// Check whether user can read file according Linux ACLs.
// As flag use either USER_CHECK or GROUP_CHECK.
int check_acl(struct permission_checker *pc, const int flag);

// Check whether user has permissions to read file according traditional
// UNIX permissions. As flag use either USER_CHECK or GROUP_CHECK.
int check_traditional(struct permission_checker *pc, const int flag);

// Returns 1 if user has permission to read file.
// Returns <0 on error and returns 0 if no permissions.
int permission_to_read(char* user, char *file_path);

#endif // PERMISSION_LINUX_H
