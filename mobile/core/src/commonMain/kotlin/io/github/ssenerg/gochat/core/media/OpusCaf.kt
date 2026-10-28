package io.github.ssenerg.gochat.core.media

/**
 * Rewraps the Opus of a browser's voice recording, WebM or Ogg, in CAF.
 * Apple's players decode Opus but open neither container; the packets are
 * copied as they are, so nothing is decoded or encoded again.
 */
object OpusCaf {
    /** [recording] in CAF, or null when it holds no Opus in WebM or Ogg. */
    fun rewrap(recording: ByteArray): ByteArray? {
        val opus = when {
            recording.startsWith(WEBM) -> Webm.read(recording)
            recording.startsWith(OGG) -> Ogg.read(recording)
            else -> null
        }
        return opus?.let(::caf)
    }

    /** How many 48 kHz samples an Opus packet holds, from its TOC (RFC 6716, 3.1). */
    fun samples(packet: ByteArray): Int {
        if (packet.isEmpty()) return 0
        val toc = packet[0].toInt() and 0xFF
        val config = toc shr 3
        val frame = when {
            config < 12 -> SILK[config and 3]
            config < 16 -> if (config and 1 == 0) 480 else 960
            else -> 120 shl (config and 3)
        }
        val frames = when (toc and 3) {
            0 -> 1
            1, 2 -> 2
            else -> if (packet.size < 2) 0 else packet[1].toInt() and 0x3F
        }
        return frame * frames
    }

    private val SILK = intArrayOf(480, 960, 1920, 2880)
    private const val MAX_SAMPLES = 5760

    // What a recording may make the parser hold, as others send them: over
    // ten minutes of Opus's shortest packets, and more tracks than any has.
    private const val MAX_PACKETS = 1 shl 18
    private const val MAX_TRACKS = 16

    private val WEBM = byteArrayOf(0x1A, 0x45, 0xDF.toByte(), 0xA3.toByte())
    private val OGG = "OggS".encodeToByteArray()
    private val OPUS_HEAD = "OpusHead".encodeToByteArray()

    private class Opus(val channels: Int, val preSkip: Int, val packets: List<ByteArray>, val granule: Long? = null)

    // The identification header: mono or stereo in the default mapping is
    // all a recorder writes.
    private fun head(p: ByteArray?, packets: List<ByteArray>, granule: Long? = null): Opus? {
        if (p == null || p.size < 19 || !p.startsWith(OPUS_HEAD) || p[8].toInt() and 0xF0 != 0) return null
        val channels = p[9].toInt() and 0xFF
        if (channels !in 1..2 || p[18].toInt() != 0) return null
        val preSkip = (p[10].toInt() and 0xFF) or ((p[11].toInt() and 0xFF) shl 8)
        return Opus(channels, preSkip, packets, granule)
    }

    private fun caf(opus: Opus): ByteArray? {
        val packets = opus.packets.filter { samples(it) in 1..MAX_SAMPLES }
        if (packets.isEmpty()) return null
        val samples = IntArray(packets.size) { samples(packets[it]) }
        // Packets of one length need no lengths of their own in the table.
        val fixed = if (samples.all { it == samples[0] }) samples[0] else 0
        val total = samples.sumOf { it.toLong() }
        val priming = minOf(opus.preSkip.toLong(), total)
        val remainder = opus.granule?.let { (total - it).coerceIn(0, minOf(total - priming, Int.MAX_VALUE.toLong())) } ?: 0
        val data = packets.sumOf { it.size.toLong() }

        val out = Out(data.toInt() + packets.size * 4 + 128)
        out.text("caff")
        out.int(0x00010000)
        out.text("desc")
        out.long(32)
        out.long(48000.0.toRawBits())
        out.text("opus")
        out.int(0)
        out.int(0)
        out.int(fixed)
        out.int(opus.channels)
        out.int(0)

        val table = Out(packets.size * 4)
        packets.forEachIndexed { i, p ->
            table.ber(p.size)
            if (fixed == 0) table.ber(samples[i])
        }
        out.text("pakt")
        out.long(24L + table.size)
        out.long(packets.size.toLong())
        out.long(total - priming - remainder)
        out.int(priming.toInt())
        out.int(remainder.toInt())
        out.bytes(table.buf, 0, table.size)

        out.text("data")
        out.long(4 + data)
        out.int(0)
        packets.forEach { out.bytes(it) }
        return out.buf.copyOf(out.size)
    }

    private object Webm {
        private const val SEGMENT = 0x18538067L
        private const val TRACKS = 0x1654AE6BL
        private const val TRACK_ENTRY = 0xAEL
        private const val TRACK_NUMBER = 0xD7L
        private const val CODEC_ID = 0x86L
        private const val CODEC_PRIVATE = 0x63A2L
        private const val CLUSTER = 0x1F43B675L
        private const val BLOCK_GROUP = 0xA0L
        private const val BLOCK = 0xA1L
        private const val SIMPLE_BLOCK = 0xA3L

        // The walk goes into these and steps over the rest, so a live
        // recording's segment and clusters of unknown size read as well.
        private val parents = setOf(SEGMENT, TRACKS, TRACK_ENTRY, CLUSTER, BLOCK_GROUP)

        private class Track(var number: Long = -1, var codec: String = "", var private: ByteArray? = null)

        fun read(b: ByteArray): Opus? {
            val tracks = ArrayList<Track>()
            val frames = ArrayList<Pair<Long, ByteArray>>()
            val r = Reader(b, 0, b.size)
            // A cut or broken recording keeps the packets before the damage.
            runCatching {
                while (r.pos < b.size) {
                    val id = r.vint(marker = true)
                    val size = r.size()
                    if (id == TRACK_ENTRY) {
                        if (tracks.size == MAX_TRACKS) break
                        tracks += Track()
                    }
                    if (id in parents) continue
                    if (size < 0 || size > b.size - r.pos) break
                    val start = r.pos
                    val end = start + size.toInt()
                    r.pos = end
                    val track = tracks.lastOrNull()
                    when (id) {
                        TRACK_NUMBER -> track?.number = Reader(b, start, end).uint()
                        CODEC_ID -> track?.codec = b.decodeToString(start, end).trimEnd('\u0000')
                        CODEC_PRIVATE -> track?.private = b.copyOfRange(start, end)
                        SIMPLE_BLOCK, BLOCK -> block(Reader(b, start, end), frames)
                    }
                }
            }
            val opus = tracks.firstOrNull { it.codec == "A_OPUS" } ?: return null
            return head(opus.private, frames.filter { it.first == opus.number }.map { it.second })
        }

        // A block's frames, of one track: laced in Xiph's way, at a fixed
        // size or in EBML's way when there are several.
        private fun block(r: Reader, out: MutableList<Pair<Long, ByteArray>>) {
            val track = r.vint()
            r.pos += 2
            val lacing = (r.u8() shr 1) and 3
            if (lacing == 0) {
                frame(out, track, r.b, r.pos, r.end)
                return
            }
            val count = r.u8() + 1
            val sizes = IntArray(count)
            when (lacing) {
                1 -> for (i in 0 until count - 1) {
                    var v: Int
                    do {
                        v = r.u8()
                        sizes[i] += v
                    } while (v == 255)
                }
                3 -> for (i in 0 until count - 1) {
                    sizes[i] = if (i == 0) r.vint().toInt() else sizes[i - 1] + r.signed().toInt()
                }
                else -> sizes.fill((r.end - r.pos) / count)
            }
            if (lacing != 2) sizes[count - 1] = r.end - r.pos - sizes.sum()
            if (sizes.any { it < 0 } || sizes.sum() != r.end - r.pos) throw Malformed
            for (size in sizes) {
                frame(out, track, r.b, r.pos, r.pos + size)
                r.pos += size
            }
        }

        // Opus has no empty packets: laced by the hundred, they would only
        // fill the memory.
        private fun frame(out: MutableList<Pair<Long, ByteArray>>, track: Long, b: ByteArray, from: Int, to: Int) {
            if (from == to) return
            if (out.size == MAX_PACKETS) throw Malformed
            out += track to b.copyOfRange(from, to)
        }
    }

    private object Ogg {
        fun read(b: ByteArray): Opus? {
            val packets = ArrayList<ByteArray>()
            var serial: Int? = null
            var granule: Long? = null
            val packet = Out(1024)
            val r = Reader(b, 0, b.size)
            runCatching {
                while (r.pos + 27 <= b.size && b.startsWith(OGG, r.pos)) {
                    val page = Reader(b, r.pos, b.size)
                    page.pos += 6
                    val position = page.le(8)
                    val pageSerial = page.le(4).toInt()
                    page.pos += 8
                    val lacing = IntArray(page.u8()) { page.u8() }
                    var at = page.pos
                    if (at + lacing.sum() > b.size) break
                    r.pos = at + lacing.sum()
                    // The first stream is the recording; any other is not.
                    if (serial == null) serial = pageSerial
                    if (pageSerial != serial) continue
                    for (len in lacing) {
                        packet.bytes(b, at, at + len)
                        at += len
                        if (len < 255) {
                            if (packet.size > 0) {
                                if (packets.size == MAX_PACKETS) throw Malformed
                                packets += packet.buf.copyOf(packet.size)
                            }
                            packet.size = 0
                        }
                    }
                    if (position >= 0) granule = position
                }
            }
            // The identification header, then the comments, then the sound.
            if (packets.size < 2) return null
            return head(packets[0], packets.subList(2, packets.size), granule)
        }
    }

    private object Malformed : Exception()

    private class Reader(val b: ByteArray, var pos: Int, val end: Int) {
        fun u8(): Int {
            if (pos >= end) throw Malformed
            return b[pos++].toInt() and 0xFF
        }

        // An EBML variable-length integer: an element's id keeps its marker
        // bit, a size or a number does not.
        fun vint(marker: Boolean = false): Long {
            val first = u8()
            val len = first.countLeadingZeroBits() - 23
            if (len > 8) throw Malformed
            var v = (if (marker) first else first and (0xFF shr len)).toLong()
            repeat(len - 1) { v = (v shl 8) or u8().toLong() }
            return v
        }

        // An element's size, -1 when unknown.
        fun size(): Long {
            val start = pos
            val v = vint()
            return if (v == (1L shl (7 * (pos - start))) - 1) -1 else v
        }

        fun signed(): Long {
            val start = pos
            val v = vint()
            return v - ((1L shl (7 * (pos - start) - 1)) - 1)
        }

        fun uint(): Long {
            var v = 0L
            while (pos < end) v = (v shl 8) or u8().toLong()
            return v
        }

        fun le(n: Int): Long {
            var v = 0L
            for (i in 0 until n) v = v or (u8().toLong() shl (8 * i))
            return v
        }
    }

    private class Out(capacity: Int) {
        var buf = ByteArray(maxOf(capacity, 16))
        var size = 0

        private fun room(n: Int) {
            if (size + n > buf.size) buf = buf.copyOf(maxOf(buf.size * 2, size + n))
        }

        fun byte(v: Int) {
            room(1)
            buf[size++] = v.toByte()
        }

        fun int(v: Int) {
            for (shift in 24 downTo 0 step 8) byte(v ushr shift)
        }

        fun long(v: Long) {
            int((v ushr 32).toInt())
            int(v.toInt())
        }

        fun text(s: String) = s.forEach { byte(it.code) }

        fun bytes(b: ByteArray, from: Int = 0, to: Int = b.size) {
            room(to - from)
            b.copyInto(buf, size, from, to)
            size += to - from
        }

        // CAF's variable-length integer: seven bits a byte, the high bit
        // set on all but the last.
        fun ber(v: Int) {
            var n = 1
            while (n < 5 && v ushr (7 * n) != 0) n++
            for (i in n - 1 downTo 1) byte(0x80 or ((v ushr (7 * i)) and 0x7F))
            byte(v and 0x7F)
        }
    }

    private fun ByteArray.startsWith(prefix: ByteArray, at: Int = 0): Boolean =
        size - at >= prefix.size && prefix.indices.all { this[at + it] == prefix[it] }
}
