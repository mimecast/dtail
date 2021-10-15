// +build linuxacl

#include "permission_linuxacl.h"

#ifdef DEBUG
void debug_print_checker(struct permission_checker *pc) {
    fprintf(stderr, "DEBUG: user_name:%s (%d)\n",
        pc->user_name, pc->uid);

    fprintf(stderr, "DEBUG: ngids:%d\n", pc->ngids);
    int j;
    for (j = 0; j < pc->ngids; j++) {
        fprintf(stderr, "DEBUG: %d", pc->gids[j]);
        struct group *gr = getgrgid(pc->gids[j]);
        if (gr != NULL)
            fprintf(stderr, " (%s)", gr->gr_name);
        fprintf(stderr, "\n");
    }

    fprintf(stderr, "DEBUG: file_path:%s (%d:%d)\n",
            pc->file_path, pc->file_stat.st_uid, pc->file_stat.st_gid);
}
#endif // DEBUG

int stat_file(struct permission_checker *pc) {
    if (stat(pc->file_path, &pc->file_stat) != 0)
      return -1;

#ifdef DEBUG
        fprintf(stderr, "DEBUG: File'%s' is owned by '%d:%d'\n",
                pc->file_path, pc->file_stat.st_uid, pc->file_stat.st_gid);
#endif

    return 0;
}

int get_user_uid(struct permission_checker *pc) {
    struct passwd *result = NULL;

    size_t bufsize = sysconf(_SC_GETPW_R_SIZE_MAX);
    if (bufsize == -1)
        bufsize = 16384;

    char *buf = malloc(bufsize);
    if (buf == NULL) {
#ifdef DEBUG
        fprintf(stderr, "DEBUG: Unabel to allocate bufer while retrieving user '%s'\n", pc->user_name);
#endif
        return -1;
    }

    int rc = getpwnam_r(pc->user_name, &pc->pw, buf, bufsize, &result);

    if (result == NULL) {
#ifdef DEBUG
        if (rc == 0) {
            fprintf(stderr, "DEBUG: No user '%s' found\n", pc->user_name);
        } else {
            fprintf(stderr, "DEBUG: Unknown error while retrieving user '%s'\n", pc->user_name);
        }
#endif

        free(buf);
        return -1;
    }

    pc->uid = pc->pw.pw_uid;

    free(buf);
    return 0;
}

int get_user_groups(struct permission_checker *pc) {
    // First assume we are in 10 groups max
    pc->ngids = 10;
    pc->gids = malloc(pc->ngids * sizeof(gid_t));

    if (pc->gids == NULL) {
#ifdef DEBUG
        fprintf(stderr, "DEBUG: Unable to allocate space for gids.");
#endif
        return -1;
    }

    // Try so many times to load group list until it fits into group array.
    while (getgrouplist(pc->user_name, pc->pw.pw_gid, pc->gids, &pc->ngids) == -1) {
        // Too many groups, enlarge group array and try again
        int newngids = pc->ngids + 100;
        size_t newsize = newngids * sizeof(gid_t);

        if (SIZE_MAX / newngids < sizeof(gid_t)) {
            // Overflow
#ifdef DEBUG
            fprintf(stderr, "DEBUG: Overflow detected.");
#endif
            return -1;
        }

        gid_t *newgids = realloc(pc->gids, newsize);
        if (newgids == NULL) {
#ifdef DEBUG
            fprintf(stderr, "DEBUG: Unable to allocate space for gids.");
#endif
            free(pc->gids);
            return -1;
        }

        pc->gids = newgids;
        pc->ngids = newngids;
    }

    return 0;
}

int is_member_of_group(struct permission_checker *pc, gid_t gid) {
    int j;
    for (j = 0; j < pc->ngids; j++)
        if (pc->gids[j] == gid)
            return 1;
    return 0;
}

int check_acl_uid_matches(uid_t uid, acl_entry_t entry) {
    int ret = -1;
    uid_t *acl_uid = acl_get_qualifier(entry);
    if (acl_uid == NULL) {
#ifdef DEBUG
        fprintf(stderr, "DEBUG: Unable to retrieve user uid from ACL entry");
#endif
        return -1;
    }

    ret = *acl_uid == uid ? 0 : -1;
#ifdef DEBUG
    fprintf(stderr, "DEBUG: ACL user match?: %d <=> %d: %d\n", *acl_uid, uid, ret);
#endif
    acl_free(acl_uid);
    return ret;
}

int check_acl_gid_matches(gid_t *gids, int ngids, acl_entry_t entry) {
    int ret = -1;
    gid_t *acl_gid = acl_get_qualifier(entry);
    if (acl_gid == NULL) {
#ifdef DEBUG
        fprintf(stderr, "DEBUG: Unable to retrieve user uid from ACL entry");
#endif
        return -1;
    }

    int j;
    for (j = 0; j < ngids; j++) {
        if (*acl_gid == gids[j]) {
#ifdef DEBUG
            fprintf(stderr, "DEBUG: User is in group %d", *acl_gid);
#endif
            ret = 0;
            break;
        }
    }

#ifdef DEBUG
    fprintf(stderr, "DEBUG: ACL group match?: %d <=> ...: %d\n", *acl_gid, ret);
#endif
    acl_free(acl_gid);
    return ret;
}

int check_acl(struct permission_checker *pc, const int flag) {
    // By default user has no read perm.
    int has_read_perm = 0;

    // By default mask tells that there are read perm. However in order to have
    // read permissions both, has_read_perm and mask_allows_read_access must be 1!
    int mask_allows_read_access = 1;

    acl_type_t type = ACL_TYPE_ACCESS;
    acl_t acl = acl_get_file(pc->file_path, type);

    if (acl == NULL)
        // Unable to retrieve ACL.
        return -1;

    // Walk through each entry of this ACL.
    int id;
    for (id = ACL_FIRST_ENTRY; ; id = ACL_NEXT_ENTRY) {
        acl_entry_t entry;
        if (acl_get_entry(acl, id, &entry) != 1)
            // No more ACL entries.
            break;

        acl_tag_t tag;
        if (acl_get_tag_type(entry, &tag) == -1)
            // Unable to retrieve ACL tag.
            return -1;

        switch (tag) {
            case ACL_USER_OBJ:
                if (flag == GROUP_CHECK)
                    continue;
#ifdef DEBUG
                fprintf(stderr, "DEBUG: ACL_USER_OBJ\n");
#endif
                // Ignore this ACL entry if user is not owner of file.
                if (pc->uid != pc->file_stat.st_uid)
                    continue;
                break;
            case ACL_USER:
                if (flag == GROUP_CHECK)
                    continue;
#ifdef DEBUG
                fprintf(stderr, "DEBUG: ACL_USER\n");
#endif
                // Ignore this ACL entry if uid does not match.
                if (check_acl_uid_matches(pc->uid, entry) != 0)
                    continue;
                break;
            case ACL_GROUP_OBJ:
                if (flag == USER_CHECK)
                    continue;
#ifdef DEBUG
                fprintf(stderr, "DEBUG: ACL_GROUP_OBJ\n");
#endif
                // Ignore ACL entry if user is not in group of file.
                if (!is_member_of_group(pc, pc->file_stat.st_gid))
                    continue;
                break;
            case ACL_GROUP:
                if (flag == USER_CHECK)
                    continue;
#ifdef DEBUG
                fprintf(stderr, "DEBUG: ACL_GROUP\n");
#endif
                // Ignore ACL entry if user is not in group of entry.
                if (check_acl_gid_matches(pc->gids, pc->ngids, entry) != 0)
                    continue;
                break;
            case ACL_OTHER:
                if (flag == GROUP_CHECK)
                    continue;
#ifdef DEBUG
                fprintf(stderr, "DEBUG: ACL_OTHER\n");
#endif
                break;
            case ACL_MASK:
#ifdef DEBUG
                fprintf(stderr, "DEBUG: ACL_MASK\n");
#endif
                break;
            default:
#ifdef DEBUG
                fprintf(stderr, "DEBUG: Unknown ACL tag\n");
#endif
                return -1;
        }

#ifdef DEBUG
        fprintf(stderr, "DEBUG: Retrieving permset\n");
#endif
        acl_permset_t permset;
        int permission;
        if (acl_get_permset(entry, &permset) == -1)
            // Unable to retrieve permset.
            return -1;

        if ((permission = acl_get_perm(permset, ACL_READ)) == -1)
            // Unable to retrieve permset value.
            return -1;

        if (permission == 1 && tag != ACL_MASK) {
#ifdef DEBUG
            fprintf(stderr, "DEBUG: ACL says user has permission to read file.\n");
#endif
            has_read_perm = 1;
        } else if (permission == 0 && tag == ACL_MASK)  {
            // Mask says that there are no permissions to read.
            mask_allows_read_access = 0;
#ifdef DEBUG
            fprintf(stderr, "DEBUG: ACL mask says no permission to read file.\n");
#endif
        }
    }

    if (has_read_perm && mask_allows_read_access) {
#ifdef DEBUG
        fprintf(stderr, "DEBUG: ACL end result: User has permission to read file.\n");
#endif
        return 1;
    }

#ifdef DEBUG
    fprintf(stderr, "DEBUG: ACL end result: User has no permission to read file.\n");
#endif
    return 0;
}

int check_traditional(struct permission_checker *pc, const int flag) {
    mode_t mode = pc->file_stat.st_mode;
    uid_t uid = pc->file_stat.st_uid;
    gid_t gid = pc->file_stat.st_gid;

    if (flag == USER_CHECK && (mode & S_IROTH)) {
#ifdef DEBUG
        fprintf(stderr, "DEBUG: Others can read file '%s'\n",
                pc->file_path);
#endif
        return 1;

    } else if (flag == USER_CHECK && (mode & S_IRUSR) && uid == pc->uid) {
#ifdef DEBUG
        fprintf(stderr, "DEBUG: User '%s' can read file '%s'\n",
                pc->user_name, pc->file_path);
#endif
        return 1;

    } else if (flag == GROUP_CHECK && (mode & S_IRGRP) && is_member_of_group(pc, gid)) {
#ifdef DEBUG
        fprintf(stderr, "DEBUG: User's '%s' group can read file '%s'\n",
                pc->user_name, pc->file_path);
#endif
        return 1;
    }

    return 0;
}

int permission_to_read(char* user_name, char *file_path) {
    int rc = -1;

#ifdef DEBUG
    fprintf(stderr, "DEBUG: User check '%s' for file '%s'\n", user_name, file_path);
#endif
    struct permission_checker pc = {
        .user_name = user_name,
        .gids = NULL,
        .ngids = 0,
        .file_path = file_path,
    };

    // Gather user's UID.
    if ((rc = get_user_uid(&pc)) == -1)
        // Could not retrieve UID.
        goto cleanup;

    // Gather file owner (user and group).
    if ((rc = stat_file(&pc)) == -1)
        // Could not stat file.
        goto cleanup;

    // Check whether there is an ACL entry which would allow the user
    // to read the file. Don't check for any groups yet. The issue with
    // groups is that it can be very slow to retrieve the list of groups
    // of a specific user when done via a remote LDAP server!
    if ((rc = check_acl(&pc, USER_CHECK)) == 1)
        // Yes, has permissions.
        goto cleanup;

    // Check whether ACLs of file could be retrieved.
    if (rc == -1) {
        if (errno != ENOTSUP)
            // Unknown error.
            goto cleanup;

        // File system does not support ACLs.
        // Fallback to traditional permissions.
        if ((rc = check_traditional(&pc, USER_CHECK)) == 1)
            // Yes, has traditional permissions.
            goto cleanup;

        if ((rc = get_user_groups(&pc)) == -1)
            // Can not retrieve user's groups.
            goto cleanup;

        rc = check_traditional(&pc, GROUP_CHECK);
        goto cleanup;
    }

    if ((rc = get_user_groups(&pc)) == -1)
        // Can not retrieve use'r groups.
        goto cleanup;

    // Check whether there is an ACL entry which would allow any of the
    // user's groups to read the file.
    rc = check_acl(&pc, GROUP_CHECK);

cleanup:
#ifdef DEBUG
    debug_print_checker(&pc);
#endif

    if (pc.ngids)
        free(pc.gids);

    return rc;
}

// vim: set tabstop=8 softtabstop=0 expandtab shiftwidth=4 smarttab
