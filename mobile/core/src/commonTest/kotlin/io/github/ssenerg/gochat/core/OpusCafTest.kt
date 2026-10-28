package io.github.ssenerg.gochat.core

import io.github.ssenerg.gochat.core.media.OpusCaf
import kotlin.test.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue

class OpusCafTest {
    @Test
    fun chromeRecordingsRewrapWhole() {
        val mono = Caf(assertNotNull(OpusCaf.rewrap(Recordings.chromeMono)))
        assertEquals(1, mono.channels)
        assertEquals(2880, mono.framesPerPacket)
        assertEquals(21, mono.packets.size)
        assertEquals(0, mono.priming)
        assertEquals(0, mono.remainder)
        assertEquals(21L * 2880, mono.valid)
        // The packets are Chrome's own, in order.
        var at = 0
        for (p in mono.packets) {
            at = Recordings.chromeMono.indexOf(p, at)
            assertTrue(at >= 0)
            at += p.size
        }

        val stereoCaf = assertNotNull(OpusCaf.rewrap(Recordings.chromeStereo))
        // As afconvert writes Opus in CAF: 48 kHz, 2880 frames a packet.
        assertEquals(
            "6361666600010000" + "6465736300000000" + "00000020" + "40e7700000000000" + "6f707573" +
                "00000000" + "00000000" + "00000b40" + "00000002" + "00000000",
            stereoCaf.copyOf(52).toHex(),
        )
        val stereo = Caf(stereoCaf)
        assertEquals(2, stereo.channels)
        assertEquals(6, stereo.packets.size)
        assertEquals(6L * 2880, stereo.valid)
    }

    @Test
    fun webmLacesAndOtherTracks() {
        val p = listOf(
            packet(19, 0, 40), // CELT 20 ms
            packet(31, 3, 300, frames = 3), // three CELT frames of 20 ms
            packet(1, 0, 20), // SILK 20 ms
            packet(19, 1, 2), // two CELT frames
            packet(13, 0, 10), // hybrid 20 ms
            packet(19, 0, 300),
            packet(19, 0, 5),
            packet(3, 0, 12), // SILK 60 ms
            packet(3, 0, 12),
        )
        val webm = ebml(0x1A45DFA3, ebml(0x4282, "webm".encodeToByteArray())) + ebml(
            0x18538067,
            ebml(0x1549A966, ebml(0x2AD7B1, byteArrayOf(0x0F, 0x42, 0x40))),
            ebml(
                0x1654AE6B,
                ebml(0xAE, ebml(0xD7, byteArrayOf(1)), ebml(0x86, "V_VP8".encodeToByteArray())),
                ebml(0xAE, ebml(0xD7, byteArrayOf(2)), ebml(0x86, "A_OPUS".encodeToByteArray()), ebml(0x63A2, opusHead(2, 312))),
            ),
            ebml(
                0x1F43B675,
                ebml(0xE7, byteArrayOf(0)),
                ebml(0xA3, block(1, 0, byteArrayOf(9, 9, 9))),
                ebml(0xA3, block(2, 0, p[0])),
                ebml(0xA0, ebml(0xA1, block(2, 1, p[1], p[2], p[3]))),
                ebml(0xEC, ByteArray(4)),
            ),
            ebml(
                0x1F43B675,
                ebml(0xE7, byteArrayOf(60)),
                ebml(0xA3, block(2, 3, p[4], p[5], p[6])),
                ebml(0xA3, block(2, 2, p[7], p[8])),
            ),
            ebml(0x1C53BB6B, ByteArray(8)),
        )
        val caf = Caf(assertNotNull(OpusCaf.rewrap(webm)))
        assertEquals(2, caf.channels)
        assertEquals(0, caf.framesPerPacket)
        assertEquals(p.size, caf.packets.size)
        p.forEachIndexed { i, want -> assertContentEquals(want, caf.packets[i]) }
        val frames = listOf(960, 2880, 960, 1920, 960, 960, 960, 2880, 2880)
        assertEquals(frames, caf.frames)
        assertEquals(312, caf.priming)
        assertEquals(frames.sum() - 312L, caf.valid)
    }

    @Test
    fun ogg() {
        val head = opusHead(1, 312)
        val tags = "OpusTags".encodeToByteArray() + ByteArray(8)
        val p1 = packet(19, 0, 30)
        val p2 = packet(19, 0, 600) // spans two pages
        val p3 = packet(19, 0, 8)
        val ogg = page(7, 0, head) +
            page(7, 0, tags) +
            page(9, 0, "OpusHead".encodeToByteArray()) + // another stream's
            page(7, -1, p1, p2.copyOf(510), last = false) +
            page(7, 3 * 960 - 200L, p2.copyOfRange(510, 600), p3)
        val caf = Caf(assertNotNull(OpusCaf.rewrap(ogg)))
        assertEquals(1, caf.channels)
        assertEquals(960, caf.framesPerPacket)
        assertEquals(3, caf.packets.size)
        assertContentEquals(p2, caf.packets[1])
        assertEquals(312, caf.priming)
        assertEquals(200, caf.remainder)
        assertEquals(3L * 960 - 312 - 200, caf.valid)
    }

    @Test
    fun anythingElseIsLeftAlone() {
        assertNull(OpusCaf.rewrap(ByteArray(0)))
        assertNull(OpusCaf.rewrap(byteArrayOf(0, 0, 0, 0x20) + "ftypM4A ".encodeToByteArray()))
        // Vorbis, five channels, and no head at all.
        val track = { codec: String, private: ByteArray? ->
            ebml(
                0x18538067,
                ebml(0x1654AE6B, ebml(0xAE, ebml(0xD7, byteArrayOf(1)), ebml(0x86, codec.encodeToByteArray()), *listOfNotNull(private?.let { ebml(0x63A2, it) }).toTypedArray())),
                ebml(0x1F43B675, ebml(0xA3, block(1, 0, packet(19, 0, 10)))),
            )
        }
        assertNotNull(OpusCaf.rewrap(ebml(0x1A45DFA3) + track("A_OPUS", opusHead(1, 0))))
        assertNull(OpusCaf.rewrap(ebml(0x1A45DFA3) + track("A_VORBIS", opusHead(1, 0))))
        assertNull(OpusCaf.rewrap(ebml(0x1A45DFA3) + track("A_OPUS", opusHead(5, 0))))
        assertNull(OpusCaf.rewrap(ebml(0x1A45DFA3) + track("A_OPUS", null)))
        assertNull(OpusCaf.rewrap(page(1, 0, "\u0001vorbis".encodeToByteArray()) + page(1, 0, ByteArray(4)) + page(1, 960, packet(19, 0, 4))))
    }

    @Test
    fun cutOrBrokenRecordingsKeepWhatIsWhole() {
        val webm = Recordings.chromeMono
        var last = 0
        for (n in 0..webm.size) {
            val packets = OpusCaf.rewrap(webm.copyOf(n))?.let { Caf(it).packets.size } ?: 0
            assertTrue(packets >= last, "cut at $n")
            last = packets
        }
        assertEquals(21, last)
        for (i in webm.indices) {
            val broken = webm.copyOf()
            broken[i] = (broken[i].toInt() xor 0xFF).toByte()
            OpusCaf.rewrap(broken)?.let { Caf(it) }
        }
    }

    @Test
    fun packetLengths() {
        assertEquals(480, OpusCaf.samples(packet(0, 0, 1)))
        assertEquals(2880, OpusCaf.samples(packet(3, 0, 1)))
        assertEquals(1920, OpusCaf.samples(packet(1, 1, 1)))
        assertEquals(480, OpusCaf.samples(packet(12, 0, 1)))
        assertEquals(1920, OpusCaf.samples(packet(15, 2, 1)))
        assertEquals(120, OpusCaf.samples(packet(16, 0, 1)))
        assertEquals(960, OpusCaf.samples(packet(19, 0, 1)))
        assertEquals(2880, OpusCaf.samples(packet(31, 3, 2, frames = 3)))
        assertEquals(0, OpusCaf.samples(packet(31, 3, 2, frames = 0)))
        assertEquals(0, OpusCaf.samples(byteArrayOf((31 shl 3 or 3).toByte())))
        assertEquals(0, OpusCaf.samples(ByteArray(0)))
    }

    /** What a CAF file of Opus says. */
    private class Caf(b: ByteArray) {
        var channels = 0
        var framesPerPacket = 0
        var valid = 0L
        var priming = 0
        var remainder = 0
        val packets = ArrayList<ByteArray>()
        val frames = ArrayList<Int>()

        init {
            assertEquals("caff", b.decodeToString(0, 4))
            var at = 8
            val sizes = ArrayList<Int>()
            while (at < b.size) {
                val type = b.decodeToString(at, at + 4)
                val size = b.long(at + 4).toInt()
                val body = at + 12
                when (type) {
                    "desc" -> {
                        assertEquals(48000.0, Double.fromBits(b.long(body)))
                        assertEquals("opus", b.decodeToString(body + 8, body + 12))
                        framesPerPacket = b.int(body + 20)
                        channels = b.int(body + 24)
                    }
                    "pakt" -> {
                        val n = b.long(body).toInt()
                        valid = b.long(body + 8)
                        priming = b.int(body + 16)
                        remainder = b.int(body + 20)
                        var p = body + 24
                        val ber = {
                            var v = 0
                            do {
                                v = (v shl 7) or (b[p].toInt() and 0x7F)
                            } while (b[p++] < 0)
                            v
                        }
                        repeat(n) {
                            sizes += ber()
                            frames += if (framesPerPacket == 0) ber() else framesPerPacket
                        }
                        assertEquals(body + size, p)
                    }
                    "data" -> {
                        var p = body + 4
                        for (s in sizes) {
                            packets += b.copyOfRange(p, p + s)
                            p += s
                        }
                        assertEquals(body + size, p)
                    }
                }
                at = body + size
            }
            assertEquals(b.size, at)
            assertEquals(frames.sumOf { it.toLong() }, valid + priming + remainder)
        }

        private fun ByteArray.int(at: Int) = (0 until 4).fold(0) { v, i -> (v shl 8) or (this[at + i].toInt() and 0xFF) }

        private fun ByteArray.long(at: Int) = (int(at).toLong() shl 32) or (int(at + 4).toLong() and 0xFFFFFFFFL)
    }

    private fun packet(config: Int, code: Int, size: Int, frames: Int = 0): ByteArray {
        val p = ByteArray(size) { (it * 7 + config).toByte() }
        p[0] = (config shl 3 or code).toByte()
        if (code == 3 && size > 1) p[1] = frames.toByte()
        return p
    }

    private fun opusHead(channels: Int, preSkip: Int) =
        "OpusHead".encodeToByteArray() + byteArrayOf(1, channels.toByte(), preSkip.toByte(), (preSkip shr 8).toByte(), 0x80.toByte(), 0xBB.toByte(), 0, 0, 0, 0, 0)

    // An EBML element, its size in eight bytes.
    private fun ebml(id: Long, vararg body: ByteArray): ByteArray {
        val content = body.fold(ByteArray(0)) { a, b -> a + b }
        val n = (64 - id.countLeadingZeroBits() + 7) / 8
        val idBytes = ByteArray(n) { (id shr (8 * (n - 1 - it))).toByte() }
        val size = ByteArray(8) { (content.size.toLong() shr (8 * (7 - it))).toByte() }
        size[0] = 0x01
        return idBytes + size + content
    }

    // A block of track 1 or 2: one frame, or several laced in Xiph's way (1),
    // at a fixed size (2) or in EBML's way (3).
    private fun block(track: Int, lacing: Int, vararg frames: ByteArray): ByteArray {
        var head = byteArrayOf((0x80 or track).toByte(), 0, 0, (0x80 or (lacing shl 1)).toByte())
        if (frames.size > 1) {
            head += (frames.size - 1).toByte()
            when (lacing) {
                1 -> for (f in frames.dropLast(1)) head += ByteArray(f.size / 255) { 0xFF.toByte() } + (f.size % 255).toByte()
                3 -> for (i in 0 until frames.size - 1) {
                    head += if (i == 0) {
                        ebmlNumber(frames[0].size.toLong())
                    } else {
                        ebmlNumber(frames[i].size - frames[i - 1].size + 8191L)
                    }
                }
            }
        }
        return frames.fold(head) { a, b -> a + b }
    }

    // A number in two bytes, as EBML lacing writes sizes.
    private fun ebmlNumber(v: Long) = byteArrayOf((0x40 or (v shr 8).toInt()).toByte(), v.toByte())

    // An Ogg page of a stream; last = false continues its last packet on the next.
    private fun page(serial: Int, granule: Long, vararg packets: ByteArray, last: Boolean = true): ByteArray {
        val lacing = ArrayList<Byte>()
        packets.forEachIndexed { i, p ->
            repeat(p.size / 255) { lacing += 0xFF.toByte() }
            if (i < packets.size - 1 || last) lacing += (p.size % 255).toByte()
        }
        val header = "OggS".encodeToByteArray() + byteArrayOf(0, 0) +
            ByteArray(8) { (granule shr (8 * it)).toByte() } +
            ByteArray(4) { (serial shr (8 * it)).toByte() } +
            ByteArray(8) + byteArrayOf(lacing.size.toByte()) + lacing.toByteArray()
        return packets.fold(header) { a, b -> a + b }
    }

    private fun ByteArray.indexOf(part: ByteArray, from: Int): Int =
        (from..size - part.size).firstOrNull { at -> part.indices.all { this[at + it] == part[it] } } ?: -1

    private fun ByteArray.toHex() = joinToString("") { (it.toInt() and 0xFF).toString(16).padStart(2, '0') }
}
