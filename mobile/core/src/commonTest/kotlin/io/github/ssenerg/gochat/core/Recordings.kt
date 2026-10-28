package io.github.ssenerg.gochat.core

import kotlin.io.encoding.Base64

/** Voice messages Chrome 152 recorded with MediaRecorder, as the web client does, at 8 kb/s. */
object Recordings {
    /** 1.26 s of a 440 Hz tone, mono: 21 packets of 60 ms in two clusters. */
    val chromeMono: ByteArray = Base64.decode(
        "GkXfo59ChoEBQveBAULygQRC84EIQoKEd2VibUKHgQRChYECGFOAZwH/////////FUmpZpkq17GDD0JATYCGQ2hyb21lV0GGQ2hy" +
        "b21lFlSua7+uvdeBAXPFhwNjH7Z2wOyDgQKGhkFfT1BVU2Oik09wdXNIZWFkAQEAAIC7AAAAAADhjbWERzuAAJ+BAWJkgSAfQ7Z1" +
        "Af/////////ngQCjwYEAAIAY4UHtb7Lp4o4Fb7MygOGwt5BXar+GdngLKmdC9Ey+kr+8jPFS6OJlGWCvMN4QIoUvqYM87C7n2HRR" +
        "tvLMo7iBADuAGOwTQkJ5dTyL+5BrSL9qOcUdR9/MsBAaTsnPYGDcTALR6ayqczet+ZKC0UNtuLBtERmFgKPBgQB1gBjsE0JCeXU5" +
        "b5ylUsCOEA6wnkkfbvZUyBBSMUTA3Brs3NqQ8kiGmU8v7h0MwDIdg/GBymf6jIva4gvP7CSjvoEAr4AY7BNCv/uQG3uuuglk3+1K" +
        "nnWu4Rs/fwMQTANkGI4lyDbeyoOQRyZQX4i1jko7tjpCAIS6o4iMqZaAo7yBAO2AGOwTQkJ5dTl3QZz3Cd6C91Ib5Au64TLMyh7S" +
        "Qo4iuLkCRYGxzwTu4m7xRmbNxwxRYJopG/m12ByjvIEBKYAY7BNCv/uQGr3mY7KjuzHX97x3A0Gq/lmGJFpq18SbE0clLHERbZO2" +
        "kvJ1SxEft65NC7RYfjYVnqO6gQFngBjsE0JCeXU6o+3hLnzG2ouVkU566TUxYcD8bpT0fUAh/WepIr5RRcHfLEQuPc/GdLwpOFH5" +
        "QKPOgQGjgBjsE0K/+5AbfnsHlMtkNIirsO3Q9Bx2csXTI71rTr7Layby/dWsBO0kTBUyYbdigQK+nQcHd7oc2AuyORU3BNDwb1WH" +
        "90s54gVgo8uBAeGAGO0WoL47lfOXSUSD3PT3atuOPknO1x9kPVyY04WPITC6w+GCrY+o8WJQEJ1Gpr9/TN6Rox36TQ5uzAA76fjT" +
        "Q8HN3fanGYCjyYECHYAY7INd8sE04txKcQs3qMG2I/BqTAP+TxTFl8FdwVWh+7PeTWh9b2/QU/B5QKToKM+ISrMcj5w2XXF04XTp" +
        "t+r2625ApTCjy4ECW4AY7PAN8sE06iNgDZuwVTT80kRR8r7PQy0BycQXwtyz/vEaT26/v6sTk46prFTNKnt+9atYiyCn8ljcuwMC" +
        "N6lR4rx7NtYpgKO/gQKXgBjs8A3ywTTihVh9kigrahlSRFzPeCBUHxCo8IIHsF8zLQTME9IutRhqos0wd+g2tRIyzvCM/SO6eILA" +
        "o8KBAs+AGOyDXfOLtXWGuu0YiIITjqZ94ItmomZgRXReAs7OWOC1vTma5uOXyz34J40H7cHunNLOdYHlK9CbgAQZhICjwoEDEIAY" +
        "7KSWwdMkmY5axaeyadpYGfDe9jsN7JWZ3dwd1xwJi8TndO0HCDrWTluoUgvMuoODn5fNAVB7M6GtfZl2gKPAgQNJgBjs8A3ywTTi" +
        "hVsCk8TFGyNAAe9nCQUPS7Z1UHj7Aa5TbxgQJIolCf24wRjhxKZLkAs0FfFTuMWJdHuVA6O9gQOFgBjs8A3ywTTihVmSvHVOa8qA" +
        "GvWJvuIuKnxENwGWt8BJUOQavaTHc/UYx7IocMuP9zoqL0i7xpNQYKO8gQPDgBjs8A3ywTTihViakxqQr7QM0sTUoxF0qRuCrmga" +
        "ezU7cBabNlEYkc+nL9cjFeSH85+GFjVrxhggH0O2dQH/////////54ID/6PAgQAAgBjs8A3zi7V1hrrtkzR+kHBSezUk0y8GWtfj" +
        "J+1XyvH7US4/HlNd8/F410Rc4HQ38NP41DWVPxZSQOzt4KPKgQA4gBjs7KJCeXU+ZLLOEiCiQTvfJ+udrxFSZLlgS43/D4FCGjxa" +
        "PyZ3A3FIwyG9Ntm7wVUIZd0LCwFPDBAKzjPGOz2g8zyXH4CjxYEAdIAY7VphImOv92KsVA2y7Zds6hQNWDTayqc24i5X9web/nR9" +
        "fuT/JlYfXnHMUX7U4LLkglReLoPFY4rdTgFpVgKwGKO/gQCygBjtRc9AUwYejffo1GQrSCSwde7VKtOAomwzbf3+Oi4chcDEHj+C" +
        "JkjALJoYVwJD6vkmMtt3imDvdNKA",
    )

    /** 0.36 s of a 660 Hz tone, stereo: 6 packets of 60 ms. */
    val chromeStereo: ByteArray = Base64.decode(
        "GkXfo59ChoEBQveBAULygQRC84EIQoKEd2VibUKHgQRChYECGFOAZwH/////////FUmpZpkq17GDD0JATYCGQ2hyb21lV0GGQ2hy" +
        "b21lFlSua7+uvdeBAXPFhzjBujxmGceDgQKGhkFfT1BVU2Oik09wdXNIZWFkAQIAAIC7AAAAAADhjbWERzuAAJ+BAmJkgSAfQ7Z1" +
        "Af/////////ngQCjyIEAAIAY4VySETz3Z+Y//QDWTiz49YkP3SuAkOVg4TSxWU5gsLcjMn5hfbuTY5+qvI05FRIhl7ThP5eZMFuv" +
        "d3eD7xYn9YRa5KO/gQA7gBjs8wTMzi/W7ftiYDy39od0yOaMyZwyC8EJgEUaSGAfFpnpSmp1FWe1Cx9QmYef6BmHWD6hXUQt/5Rg" +
        "o7yBAHSAGOzofEgHf5qx0DsowQjX9/J93YDXsnzj7xeBaSDhXizlRqoklcbQfrhaWMKQXfjiP/IUpGtYGBCjw4EAr4AY7OvMgiYl" +
        "qJvidho6lwQI2y2jJOyhNbiug8GJz1EbjxFk3QaaQb7ZYWjwk5sDPteSBBEUxY+NO8nI8D5bWSCjvIEA7oAY7OvMgiYlqJvidho3" +
        "6LCyzamnfBdu+jSsmn217bhAjbswZb+PNEp9i+PZWb/nASzmWRahBJhA8KO2gQEpgBjs8wTMzkmIdgrjt/U0ULz41qDY0sA5TMdZ" +
        "0Bf9i9RPTYr6Pjj005F9XIenk8b5N24S",
    )
}
