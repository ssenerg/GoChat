package io.github.ssenerg.gochat.core.api

import gochat.v1.*

/** The calls of the chat API the app makes, typed. */
class GoChatApi(val client: ApiClient) {
    private suspend fun <T : Any> auth(m: String, req: com.squareup.wire.Message<*, *>, res: com.squareup.wire.ProtoAdapter<T>) =
        client.call("gochat.v1.AuthService/$m", req, res)

    private suspend fun <T : Any> users(m: String, req: com.squareup.wire.Message<*, *>, res: com.squareup.wire.ProtoAdapter<T>) =
        client.call("gochat.v1.UserService/$m", req, res)

    private suspend fun <T : Any> teams(m: String, req: com.squareup.wire.Message<*, *>, res: com.squareup.wire.ProtoAdapter<T>) =
        client.call("gochat.v1.TeamService/$m", req, res)

    private suspend fun <T : Any> channels(m: String, req: com.squareup.wire.Message<*, *>, res: com.squareup.wire.ProtoAdapter<T>) =
        client.call("gochat.v1.ChannelService/$m", req, res)

    private suspend fun <T : Any> messages(m: String, req: com.squareup.wire.Message<*, *>, res: com.squareup.wire.ProtoAdapter<T>) =
        client.call("gochat.v1.MessageService/$m", req, res)

    // Accounts.

    suspend fun signupInfo(invite: String = "") =
        auth("GetSignupInfo", GetSignupInfoRequest(invite_token = invite), GetSignupInfoResponse.ADAPTER)

    suspend fun signUp(req: SignUpRequest) = auth("SignUp", req, SignUpResponse.ADAPTER)

    suspend fun login(login: String, password: String) =
        auth("Login", LoginRequest(login = login, password = password), LoginResponse.ADAPTER)

    suspend fun verifyMfa(mfaToken: String, code: String) =
        auth("VerifyMFA", VerifyMFARequest(mfa_token = mfaToken, code = code), VerifyMFAResponse.ADAPTER).session

    suspend fun logout() {
        auth("Logout", LogoutRequest(), LogoutResponse.ADAPTER)
    }

    suspend fun me() = auth("Me", MeRequest(), MeResponse.ADAPTER)

    suspend fun changePassword(current: String, new: String) {
        auth("ChangePassword", ChangePasswordRequest(current_password = current, new_password = new), ChangePasswordResponse.ADAPTER)
    }

    // People.

    suspend fun user(id: String) = users("GetUser", GetUserRequest(user_id = id), GetUserResponse.ADAPTER).user

    suspend fun updateProfile(displayName: String) =
        users("UpdateProfile", UpdateProfileRequest(display_name = displayName), UpdateProfileResponse.ADAPTER).user

    suspend fun presence(ids: List<String>) =
        users("GetPresence", GetPresenceRequest(user_ids = ids), GetPresenceResponse.ADAPTER)

    suspend fun setStatus(status: Status, dndMinutes: Int = 0) =
        users("SetStatus", SetStatusRequest(status = status, dnd_minutes = dndMinutes), SetStatusResponse.ADAPTER).user

    suspend fun setCustomStatus(emoji: String, text: String, minutes: Int = 0) =
        users("SetCustomStatus", SetCustomStatusRequest(emoji = emoji, text = text, minutes = minutes), SetCustomStatusResponse.ADAPTER).user

    suspend fun removePhoto() = users("RemovePhoto", RemovePhotoRequest(), RemovePhotoResponse.ADAPTER).user

    // Teams.

    suspend fun listTeams() = teams("ListTeams", ListTeamsRequest(), ListTeamsResponse.ADAPTER).teams

    suspend fun createTeam(name: String, displayName: String) =
        teams("CreateTeam", CreateTeamRequest(name = name, display_name = displayName), CreateTeamResponse.ADAPTER).team

    suspend fun teamMembers(teamId: String, query: String = "", limit: Int = 50) =
        teams("ListTeamMembers", ListTeamMembersRequest(team_id = teamId, query = query, limit = limit), ListTeamMembersResponse.ADAPTER).members

    suspend fun createInvite(teamId: String) =
        teams("CreateInvite", CreateInviteRequest(team_id = teamId), CreateInviteResponse.ADAPTER)

    suspend fun invite(token: String) = teams("GetInvite", GetInviteRequest(token = token), GetInviteResponse.ADAPTER)

    suspend fun acceptInvite(token: String) =
        teams("AcceptInvite", AcceptInviteRequest(token = token), AcceptInviteResponse.ADAPTER).team

    // Channels.

    suspend fun listChannels(teamId: String) =
        channels("ListChannels", ListChannelsRequest(team_id = teamId), ListChannelsResponse.ADAPTER).channels

    suspend fun browseChannels(teamId: String) =
        channels("BrowseChannels", BrowseChannelsRequest(team_id = teamId), BrowseChannelsResponse.ADAPTER)

    suspend fun createChannel(req: CreateChannelRequest) = channels("CreateChannel", req, CreateChannelResponse.ADAPTER).channel

    suspend fun channel(id: String) = channels("GetChannel", GetChannelRequest(channel_id = id), GetChannelResponse.ADAPTER).channel

    suspend fun joinChannel(id: String) = channels("JoinChannel", JoinChannelRequest(channel_id = id), JoinChannelResponse.ADAPTER).channel

    suspend fun leaveChannel(id: String) {
        channels("LeaveChannel", LeaveChannelRequest(channel_id = id), LeaveChannelResponse.ADAPTER)
    }

    suspend fun channelMembers(id: String) =
        channels("ListChannelMembers", ListChannelMembersRequest(channel_id = id), ListChannelMembersResponse.ADAPTER).members

    suspend fun addChannelMember(channelId: String, userId: String) {
        channels("AddChannelMember", AddChannelMemberRequest(channel_id = channelId, user_id = userId), AddChannelMemberResponse.ADAPTER)
    }

    suspend fun removeChannelMember(channelId: String, userId: String) {
        channels("RemoveChannelMember", RemoveChannelMemberRequest(channel_id = channelId, user_id = userId), RemoveChannelMemberResponse.ADAPTER)
    }

    /** Opens the conversation with [userId] in [teamId]; each team has its own. */
    suspend fun openDirect(teamId: String, userId: String) =
        channels("OpenDirectChannel", OpenDirectChannelRequest(user_id = userId, team_id = teamId), OpenDirectChannelResponse.ADAPTER).channel

    suspend fun openGroup(teamId: String, userIds: List<String>) =
        channels("OpenGroupChannel", OpenGroupChannelRequest(user_ids = userIds, team_id = teamId), OpenGroupChannelResponse.ADAPTER).channel

    /** Clears a direct conversation for the caller, or with [everyone] deletes it for both people. */
    suspend fun deleteConversation(channelId: String, everyone: Boolean) {
        channels("DeleteConversation", DeleteConversationRequest(channel_id = channelId, everyone = everyone), DeleteConversationResponse.ADAPTER)
    }

    suspend fun markRead(channelId: String, messageId: String) {
        channels("MarkChannelRead", MarkChannelReadRequest(channel_id = channelId, message_id = messageId), MarkChannelReadResponse.ADAPTER)
    }

    suspend fun setMuted(channelId: String, muted: Boolean) {
        channels("SetChannelMuted", SetChannelMutedRequest(channel_id = channelId, muted = muted), SetChannelMutedResponse.ADAPTER)
    }

    suspend fun mutedChannels() = channels("ListMutedChannels", ListMutedChannelsRequest(), ListMutedChannelsResponse.ADAPTER).channel_ids

    suspend fun updateChannel(id: String, displayName: String, name: String, purpose: String) =
        channels(
            "UpdateChannel",
            UpdateChannelRequest(channel_id = id, display_name = displayName, name = name, purpose = purpose),
            UpdateChannelResponse.ADAPTER,
        ).channel

    suspend fun setPrivacy(id: String, private: Boolean) =
        channels("SetChannelPrivacy", SetChannelPrivacyRequest(channel_id = id, private_ = private), SetChannelPrivacyResponse.ADAPTER).channel

    suspend fun archive(id: String, archived: Boolean) =
        channels("ArchiveChannel", ArchiveChannelRequest(channel_id = id, archived = archived), ArchiveChannelResponse.ADAPTER).channel

    suspend fun setVoiceMessages(id: String, enabled: Boolean) =
        channels("SetChannelVoice", SetChannelVoiceRequest(channel_id = id, enabled = enabled), SetChannelVoiceResponse.ADAPTER).channel

    // Messages.

    suspend fun post(channelId: String, content: String, attachmentIds: List<String> = emptyList(), rootId: String = "") =
        messages(
            "PostMessage",
            PostMessageRequest(channel_id = channelId, content = content, attachment_ids = attachmentIds, root_id = rootId),
            PostMessageResponse.ADAPTER,
        ).message

    suspend fun edit(m: Message, content: String) =
        messages(
            "EditMessage",
            EditMessageRequest(channel_id = m.channel_id, message_id = m.id, content = content, root_id = m.root_id),
            EditMessageResponse.ADAPTER,
        ).message

    suspend fun delete(m: Message) {
        messages("DeleteMessage", DeleteMessageRequest(channel_id = m.channel_id, message_id = m.id, root_id = m.root_id), DeleteMessageResponse.ADAPTER)
    }

    suspend fun history(channelId: String, beforeId: String = "", limit: Int = 50, afterId: String = "") =
        messages(
            "ListMessages",
            ListMessagesRequest(channel_id = channelId, before_id = beforeId, limit = limit, after_id = afterId),
            ListMessagesResponse.ADAPTER,
        )

    suspend fun message(channelId: String, messageId: String, rootId: String = "") =
        messages("GetMessage", GetMessageRequest(channel_id = channelId, message_id = messageId, root_id = rootId), GetMessageResponse.ADAPTER).message

    suspend fun typing(channelId: String) {
        messages("SendTyping", SendTypingRequest(channel_id = channelId), SendTypingResponse.ADAPTER)
    }

    suspend fun thread(channelId: String, rootId: String, beforeId: String = "", limit: Int = 100) =
        messages("ListThread", ListThreadRequest(channel_id = channelId, root_id = rootId, before_id = beforeId, limit = limit), ListThreadResponse.ADAPTER)

    suspend fun react(m: Message, emoji: String, remove: Boolean) =
        messages(
            "React",
            ReactRequest(channel_id = m.channel_id, message_id = m.id, root_id = m.root_id, emoji = emoji, remove = remove),
            ReactResponse.ADAPTER,
        ).reactions

    suspend fun search(teamId: String, query: String, cursor: String = "", limit: Int = 25) =
        messages("SearchMessages", SearchMessagesRequest(team_id = teamId, query = query, cursor = cursor, limit = limit), SearchMessagesResponse.ADAPTER)

    suspend fun threads(teamId: String, offset: Int = 0, limit: Int = 30) =
        messages("ListThreads", ListThreadsRequest(team_id = teamId, offset = offset, limit = limit), ListThreadsResponse.ADAPTER)

    suspend fun markThreadRead(channelId: String, rootId: String) {
        messages("MarkThreadRead", MarkThreadReadRequest(channel_id = channelId, root_id = rootId), MarkThreadReadResponse.ADAPTER)
    }

    suspend fun follow(channelId: String, rootId: String, follow: Boolean) {
        messages("FollowThread", FollowThreadRequest(channel_id = channelId, root_id = rootId, follow = follow), FollowThreadResponse.ADAPTER)
    }

    suspend fun pin(m: Message, pinned: Boolean) =
        messages("PinMessage", PinMessageRequest(channel_id = m.channel_id, message_id = m.id, root_id = m.root_id, pinned = pinned), PinMessageResponse.ADAPTER).message

    suspend fun pinned(channelId: String) = messages("ListPinned", ListPinnedRequest(channel_id = channelId), ListPinnedResponse.ADAPTER).messages

    suspend fun save(m: Message, saved: Boolean) =
        messages("SaveMessage", SaveMessageRequest(channel_id = m.channel_id, message_id = m.id, root_id = m.root_id, saved = saved), SaveMessageResponse.ADAPTER).message

    suspend fun saved(teamId: String) = messages("ListSaved", ListSavedRequest(team_id = teamId), ListSavedResponse.ADAPTER).hits

    suspend fun savedIds() = messages("ListSaved", ListSavedRequest(ids_only = true), ListSavedResponse.ADAPTER).message_ids

    // Bots: their buttons and commands.

    /** Presses a button of a bot's message; the bot's answer comes as a CallbackAnswer with the returned query id. */
    suspend fun pressButton(m: Message, data: String) =
        messages(
            "PressButton",
            PressButtonRequest(channel_id = m.channel_id, message_id = m.id, root_id = m.root_id, data_ = data),
            PressButtonResponse.ADAPTER,
        ).query_id

    suspend fun channelCommands(channelId: String) =
        client.call("gochat.v1.BotService/ListChannelCommands", ListChannelCommandsRequest(channel_id = channelId), ListChannelCommandsResponse.ADAPTER)
            .commands

    // Updates: what the socket missed.

    private suspend fun <T : Any> updates(m: String, req: com.squareup.wire.Message<*, *>, res: com.squareup.wire.ProtoAdapter<T>) =
        client.call("gochat.v1.UpdatesService/$m", req, res)

    suspend fun difference(pts: Long, limit: Int = DIFFERENCE_LIMIT) =
        updates("GetDifference", GetDifferenceRequest(pts = pts, limit = limit), GetDifferenceResponse.ADAPTER)

    suspend fun channelDifference(channelId: String, pts: Long, limit: Int = DIFFERENCE_LIMIT) =
        updates(
            "GetChannelDifference",
            GetChannelDifferenceRequest(channel_id = channelId, pts = pts, limit = limit),
            GetChannelDifferenceResponse.ADAPTER,
        )

    // Voice channels: who talks where, and the calls VoiceCall makes to talk.

    private suspend fun <T : Any> voice(m: String, req: com.squareup.wire.Message<*, *>, res: com.squareup.wire.ProtoAdapter<T>) =
        client.call("gochat.v1.VoiceService/$m", req, res)

    suspend fun voiceStates(teamId: String) = voice("ListVoiceStates", ListVoiceStatesRequest(team_id = teamId), ListVoiceStatesResponse.ADAPTER).channels

    /** The voice server's address and a token to identify there with. */
    suspend fun joinVoice(channelId: String) = voice("JoinVoice", JoinVoiceRequest(channel_id = channelId), JoinVoiceResponse.ADAPTER)

    suspend fun leaveVoice() {
        voice("LeaveVoice", LeaveVoiceRequest(), LeaveVoiceResponse.ADAPTER)
    }

    suspend fun updateVoiceState(selfMute: Boolean, selfDeaf: Boolean) {
        voice("UpdateVoiceState", UpdateVoiceStateRequest(self_mute = selfMute, self_deaf = selfDeaf), UpdateVoiceStateResponse.ADAPTER)
    }

    /** Takes someone out of a voice channel's talk, for its moderators. */
    suspend fun disconnectVoiceMember(channelId: String, userId: String) {
        voice("DisconnectVoiceMember", DisconnectVoiceMemberRequest(channel_id = channelId, user_id = userId), DisconnectVoiceMemberResponse.ADAPTER)
    }

    companion object {
        /** The most updates the server answers at once. */
        const val DIFFERENCE_LIMIT = 100
    }
}
