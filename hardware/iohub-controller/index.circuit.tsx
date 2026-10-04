/**
 * IoHub-C1 — generic ESP32-S3 industrial controller
 *
 * Field I/O for a plant, talking to the IoT Hub in ../../iot-hub over WiFi
 * (MQTT) or RS-485 (Modbus RTU, which the hub's connector already speaks).
 *
 *   Supply    9-36 V DC (nominal 24 V), reverse-polarity and transient protected
 *   MCU       ESP32-S3-WROOM-1-N16R8 (WiFi/BLE, 16 MB flash, 8 MB PSRAM)
 *   DI        8 x 24 V opto-isolated digital inputs, sinking
 *   DO        4 x SPDT relay outputs, 5 A changeover brought out
 *   AI        4 x 4-20 mA, 16-bit (ADS1115), 150 ohm shunts
 *   Serial    RS-485 half duplex, switchable 120 ohm termination
 *   Console   USB-C (native USB) and a 3-pin UART0 header
 *
 * The channel counts are arrays: change DI_PINS / RLY_PINS / AI_CHANNELS and
 * the schematic, PCB and BOM follow.
 *
 * Only PCB positions are given below. Each <schematicsheet> lays its own
 * children out, so schX/schY on anything inside one is ignored.
 *
 * Floorplan, on a 160 x 100 mm board (x -80..80, y -50..50). Every position is
 * picked from the real courtyard of the footprint it belongs to, so the bands
 * never touch:
 *
 *   y  40       field terminals and the USB/console connectors, top edge
 *   y  21..35   digital-input front end, 8 columns on a 9.5 mm pitch
 *   y -10..18   power (left, fed from a terminal on the left edge), the MCU
 *               (centre), analog and RS-485 (right)
 *   y -30..-14  relays, on a 21 mm pitch
 *   y -44       relay terminals, bottom edge
 */
import { ESP32_S3_WROOM_1_N16R8 } from "./imports/ESP32_S3_WROOM_1_N16R8"
import { SP3485EN_L_TR } from "./imports/SP3485EN_L_TR"
import { ULN2003ADR } from "./imports/ULN2003ADR"
import { ADS1115IDGSR } from "./imports/ADS1115IDGSR"
import { EL357N_C__TA__G } from "./imports/EL357N_C__TA__G"
import { SRD_05VDC_SL_C } from "./imports/SRD_05VDC_SL_C"
import { MP1584EN_LF_Z } from "./imports/MP1584EN_LF_Z"

// ---------------------------------------------------------------- pin budget
//
// Avoided on purpose: IO35/36/37 are the octal PSRAM bus on an -R8 module,
// IO0/IO3/IO45/IO46 are strapping pins, IO19/IO20 are native USB, and
// TXD0/RXD0 stay free for the console header.
const DI_PINS = ["IO4", "IO5", "IO6", "IO7", "IO10", "IO11", "IO12", "IO13"]
const RLY_PINS = ["IO38", "IO39", "IO40", "IO41"]
const AI_CHANNELS = ["AIN0", "AIN1", "AIN2", "AIN3"]
const I2C = { sda: "IO8", scl: "IO9" }
const RS485 = { tx: "IO17", rx: "IO18", de: "IO16" }
const LEDS = { run: "IO42", comm: "IO21" }

// Field wiring lands on 5.08 mm screw terminals.
const TERM = (ways: number) => `pinrow${ways}_p5.08mm`

// The 0603 indicator the parts engine picks (JLCPCB C965799) has its cathode
// on pin 1. Say so once instead of per instance, then wire every LED by
// anode/cathode so the polarity cannot drift.
const LED_PINS = { pin1: "cathode", pin2: "anode" } as const

// The MP1584's stock footprint drops a 2x2 via field inside the thermal pad,
// which the DRC reads as four vias punched through an SMD pad. Same pad, no
// stitching vias: at the ~0.3 W this rail dissipates the copper pour under the
// pad is enough. Put the vias back (and waive the check) if the load grows.
const MP1584_FOOTPRINT =
  "dfn8_thermalpad2mmx2mm_pillpads_w7.58mm_pw0.57mm_pl2.04mm_pin1location(leftside,bottom)"

/** One 24 V opto-isolated input: dropper, reverse clamp, opto, pull-up. */
const DigitalInput = ({
  n,
  gpio,
  terminal,
  pcbX,
}: { n: number; gpio: string; terminal: string; pcbX: number }) => (
  <group>
    {/* 4.7k at 24 V gives ~4.8 mA through the LED, 0.11 W in a 1206. */}
    <resistor
      name={`R${19 + n}`}
      resistance="4.7k"
      footprint="1206"
      connections={{ pin1: terminal, pin2: `OK${n}.pin1` }}
      pcbX={pcbX}
      pcbY={35}
    />
    {/* Reverse clamp: the LED only tolerates ~6 V backwards. */}
    <diode
      name={`D${9 + n}`}
      footprint="sod123"
      connections={{ pin1: "net.FGND", pin2: `OK${n}.pin1` }}
      pcbX={pcbX}
      pcbY={31}
    />
    <EL357N_C__TA__G
      name={`OK${n}`}
      connections={{ pin2: "net.FGND", pin3: "net.GND", pin4: `net.DI${n}` }}
      pcbX={pcbX}
      pcbY={26}
    />
    {/* Pull-up on the logic side: the input reads low when energised. */}
    <resistor
      name={`R${29 + n}`}
      resistance="10k"
      footprint="0603"
      connections={{ pin1: "net.V3_3", pin2: `net.DI${n}` }}
      pcbX={pcbX}
      pcbY={21}
    />
    <trace from={`net.DI${n}`} to={`U3.${gpio}`} />
  </group>
)

export default () => (
  <board width="160mm" height="100mm" layers={2}>
    {/* ===================================================== 0. POWER ===== */}
    <schematicsheet name="power" displayName="Power — 24 V in, 5 V, 3V3" sheetIndex={0}>
      {/* Supply terminal on the left edge, the way a DIN-rail box is wired:
          field power in on one side, signals along the top and bottom. */}
      <connector
        name="X1"
        footprint={TERM(3)}
        pinLabels={{ pin1: "V24_IN", pin2: "GND_IN", pin3: "EARTH" }}
        connections={{ GND_IN: "net.GND" }}
        pcbX={-76}
        pcbY={10}
        pcbRotation={90}
      />
      {/* Series Schottky: survives reversed field wiring, which happens. */}
      <diode
        name="D1"
        footprint="smb"
        connections={{ pin1: "X1.V24_IN", pin2: "net.V24" }}
        pcbX={-66}
        pcbY={14}
      />
      {/* Bidirectional TVS across the rail for inductive transients. */}
      <diode
        name="D2"
        footprint="smb"
        connections={{ pin1: "net.V24", pin2: "net.GND" }}
        pcbX={-58}
        pcbY={14}
      />
      <capacitor
        name="C1"
        capacitance="100uF"
        maxVoltageRating="50V"
        footprint="electrolytic_d6.3mm_p2.5mm"
        connections={{ pin1: "net.V24", pin2: "net.GND" }}
        pcbX={-50}
        pcbY={14}
      />
      <capacitor
        name="C2"
        capacitance="100nF"
        footprint="0603"
        connections={{ pin1: "net.V24", pin2: "net.GND" }}
        pcbX={-43}
        pcbY={14}
      />

      {/* 24 V -> 5 V buck. FB is 0.8 V: 52.3k/10k gives 4.98 V. */}
      <MP1584EN_LF_Z
        name="U1"
        footprint={MP1584_FOOTPRINT}
        connections={{
          VIN: "net.V24",
          EN: "net.V24",
          GND: "net.GND",
          EP: "net.GND",
          SW: "net.SW5",
          BST: "C3.pin2",
          FB: "net.FB5",
          COMP: "R3.pin1",
          FREQ: "R4.pin1",
        }}
        pcbX={-68}
        pcbY={4}
      />
      <capacitor name="C3" capacitance="10nF" footprint="0603" connections={{ pin1: "net.SW5" }} pcbX={-61} pcbY={4} pcbRotation={180} />
      <inductor name="L1" inductance="10uH" footprint="1210" connections={{ pin1: "net.SW5", pin2: "net.V5" }} pcbX={-55} pcbY={4} />
      <resistor name="R1" resistance="52.3k" footprint="0603" connections={{ pin1: "net.V5", pin2: "net.FB5" }} pcbX={-49} pcbY={4} />
      <resistor name="R2" resistance="10k" footprint="0603" connections={{ pin1: "net.FB5", pin2: "net.GND" }} pcbX={-44} pcbY={4} />
      <capacitor name="C5" capacitance="22uF" maxVoltageRating="16V" footprint="1206" connections={{ pin1: "net.V5", pin2: "net.GND" }} pcbX={-38} pcbY={4} />
      <capacitor name="C6" capacitance="22uF" maxVoltageRating="16V" footprint="1206" connections={{ pin1: "net.V5", pin2: "net.GND" }} pcbX={-32} pcbY={4} />
      <resistor name="R3" resistance="10k" footprint="0603" connections={{ pin2: "C4.pin1" }} pcbX={-26} pcbY={4} />
      <capacitor name="C4" capacitance="2.2nF" footprint="0603" connections={{ pin2: "net.GND" }} pcbX={-21} pcbY={4} />
      <resistor name="R4" resistance="100k" footprint="0603" connections={{ pin2: "net.GND" }} pcbX={-16} pcbY={4} />

      {/* 5 V -> 3V3 for the MCU side. SOT-223 handles the ~0.6 W. */}
      <chip
        name="U2"
        footprint="sot223"
        pinLabels={{ pin1: "GND", pin2: "VOUT", pin3: "VIN", pin4: "VOUT_TAB" }}
        pinAttributes={{ VIN: { requiresPower: true }, GND: { requiresGround: true } }}
        supplierPartNumbers={{ jlcpcb: ["C6186"] }}
        connections={{ GND: "net.GND", VIN: "net.V5", VOUT: "net.V3_3", VOUT_TAB: "net.V3_3" }}
        pcbX={-70}
        pcbY={-6}
      />
      <capacitor name="C7" capacitance="10uF" footprint="0805" connections={{ pin1: "net.V5", pin2: "net.GND" }} pcbX={-61} pcbY={-6} />
      <capacitor name="C8" capacitance="22uF" footprint="0805" connections={{ pin1: "net.V3_3", pin2: "net.GND" }} pcbX={-55} pcbY={-6} />
      <capacitor name="C9" capacitance="100nF" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.GND" }} pcbX={-49} pcbY={-6} />

      {/* Power-good indicator on the 3V3 rail. */}
      <led name="LED1" color="green" footprint="0603" pinLabels={LED_PINS} connections={{ anode: "net.V3_3", cathode: "R5.pin1" }} pcbX={-44} pcbY={-6} />
      <resistor name="R5" resistance="2k" footprint="0603" connections={{ pin2: "net.GND" }} pcbX={-39} pcbY={-6} />
    </schematicsheet>

    {/* ======================================================= 1. MCU ===== */}
    <schematicsheet name="mcu" displayName="MCU — ESP32-S3, USB, console" sheetIndex={1}>
      <ESP32_S3_WROOM_1_N16R8
        name="U3"
        connections={{
          "3V3": "net.V3_3",
          GND1: "net.GND",
          GND2: "net.GND",
          GND3: "net.GND",
          EN: "net.EN",
          IO0: "net.BOOT",
          IO19: "net.USB_DM",
          IO20: "net.USB_DP",
        }}
        pcbX={0}
        pcbY={2}
      />
      <capacitor name="C10" capacitance="22uF" footprint="0805" connections={{ pin1: "net.V3_3", pin2: "net.GND" }} pcbX={14} pcbY={2} />
      <capacitor name="C11" capacitance="100nF" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.GND" }} pcbX={14} pcbY={-2} />
      <capacitor name="C12" capacitance="100nF" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.GND" }} pcbX={14} pcbY={-6} />

      {/* EN: 10k pull-up and 1uF so the module comes out of reset cleanly. */}
      <resistor name="R6" resistance="10k" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.EN" }} pcbX={-14} pcbY={0} />
      <capacitor name="C13" capacitance="1uF" footprint="0603" connections={{ pin1: "net.EN", pin2: "net.GND" }} pcbX={-14} pcbY={-4} />
      <pushbutton name="SW1" footprint="smdpushbutton" connections={{ pin1: "net.EN", pin2: "net.GND" }} pcbX={-14} pcbY={-9} />

      {/* BOOT: hold low through reset to enter the ROM loader. */}
      <resistor name="R7" resistance="10k" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.BOOT" }} pcbX={-22} pcbY={0} pcbRotation={180} />
      <pushbutton name="SW2" footprint="smdpushbutton" connections={{ pin1: "net.BOOT", pin2: "net.GND" }} pcbX={-22} pcbY={-9} />

      {/* USB-C overhanging the top edge so a cable can reach it, rotated so
          the receptacle opens away from the board. Both sides of the
          reversible connector are wired, plus a Schottky so a laptop can run
          the board on the bench with no 24 V field supply. */}
      <connector name="J1" standard="usb_c" pcbX={-5} pcbY={45.5} pcbRotation={180} />
      <trace from="J1.DP1" to="net.USB_DP" />
      <trace from="J1.DP2" to="net.USB_DP" />
      <trace from="J1.DM1" to="net.USB_DM" />
      <trace from="J1.DM2" to="net.USB_DM" />
      <trace from="J1.GND1" to="net.GND" />
      <trace from="J1.GND2" to="net.GND" />
      <trace from="J1.SHELL1" to="net.GND" />
      <trace from="J1.VBUS2" to="D3.pin1" />
      <resistor name="R8" resistance="5.1k" footprint="0603" connections={{ pin1: "J1.CC1", pin2: "net.GND" }} pcbX={2} pcbY={34} />
      <resistor name="R9" resistance="5.1k" footprint="0603" connections={{ pin1: "J1.CC2", pin2: "net.GND" }} pcbX={2} pcbY={31} />
      <diode name="D3" footprint="sma" connections={{ pin1: "J1.VBUS1", pin2: "net.V5" }} pcbX={2} pcbY={26} />

      {/* Status LEDs driven by firmware: RUN heartbeat, COMM on hub traffic. */}
      <led name="LED2" color="green" footprint="0603" pinLabels={LED_PINS} connections={{ anode: `U3.${LEDS.run}`, cathode: "R10.pin1" }} pcbX={20} pcbY={10} />
      <resistor name="R10" resistance="1k" footprint="0603" connections={{ pin2: "net.GND" }} pcbX={20} pcbY={7} />
      <led name="LED3" color="blue" footprint="0603" pinLabels={LED_PINS} connections={{ anode: `U3.${LEDS.comm}`, cathode: "R11.pin1" }} pcbX={26} pcbY={10} />
      <resistor name="R11" resistance="1k" footprint="0603" connections={{ pin2: "net.GND" }} pcbX={26} pcbY={7} />

      {/* UART0 console header: the serial log when USB is not convenient. */}
      <connector
        name="J2"
        footprint="pinrow3_p2.54mm"
        pinLabels={{ pin1: "GND", pin2: "TX", pin3: "RX" }}
        connections={{ GND: "net.GND", TX: "U3.TXD0", RX: "U3.RXD0" }}
        pcbX={6}
        pcbY={40}
      />
    </schematicsheet>

    {/* ==================================== 2-3. DIGITAL INPUTS (x8) ====== */}
    {/* Four channels to a sheet: eight on one sheet overflows the drawing
        area, and each terminal block gets its own page this way.
        The field side has its own common (FGND); it never meets logic GND. */}
    <schematicsheet name="di_1_4" displayName="Digital inputs 1-4 — 24 V opto-isolated" sheetIndex={2}>
      <connector
        name="X2"
        footprint={TERM(5)}
        pinLabels={{ pin1: "DI1", pin2: "DI2", pin3: "DI3", pin4: "DI4", pin5: "FGND" }}
        connections={{ FGND: "net.FGND" }}
        pcbX={-62}
        pcbY={40}
      />
      {DI_PINS.slice(0, 4).map((gpio, i) => (
        <DigitalInput
          key={i}
          n={i + 1}
          gpio={gpio}
          terminal={`X2.DI${i + 1}`}
          pcbX={-74 + i * 9.5}
        />
      ))}
    </schematicsheet>
    <schematicsheet name="di_5_8" displayName="Digital inputs 5-8 — 24 V opto-isolated" sheetIndex={3}>
      <connector
        name="X3"
        footprint={TERM(5)}
        pinLabels={{ pin1: "DI5", pin2: "DI6", pin3: "DI7", pin4: "DI8", pin5: "FGND" }}
        connections={{ FGND: "net.FGND" }}
        pcbX={-32}
        pcbY={40}
      />
      {DI_PINS.slice(4).map((gpio, i) => (
        <DigitalInput
          key={i}
          n={i + 5}
          gpio={gpio}
          terminal={`X3.DI${i + 5}`}
          pcbX={-36 + i * 9.5}
        />
      ))}
    </schematicsheet>

    {/* ======================================== 4. RELAY OUTPUTS (x4) ===== */}
    <schematicsheet name="do" displayName="Relay outputs — 4 x SPDT" sheetIndex={4}>
      {/* The ULN2003 sinks the coils and holds the flyback diodes (COM). */}
      <ULN2003ADR name="U4" connections={{ E: "net.GND", COM: "net.V5" }} pcbX={30} pcbY={-22} />
      {RLY_PINS.map((gpio, i) => {
        const n = i + 1
        // The relay courtyard is 19.7 mm wide, so a 21 mm pitch, with each
        // relay's terminal directly below it on the bottom edge and its
        // indicator in the gap between the two.
        const px = -57 + i * 21
        return (
          <group key={n}>
            <trace from={`U3.${gpio}`} to={`U4.${n}B`} />
            {/* Coil: pins 2 and 3 of the SRD relay, contacts on the far side. */}
            <SRD_05VDC_SL_C
              name={`K${n}`}
              connections={{ pin2: "net.V5", pin3: `U4.${n}C` }}
              pcbX={px}
              pcbY={-22}
            />
            {/* Full changeover to the terminal: common plus both contacts. */}
            <connector
              name={`X${10 + n}`}
              footprint={TERM(3)}
              pinLabels={{ pin1: "COM", pin2: "NO", pin3: "NC" }}
              connections={{ COM: `K${n}.pin5`, NO: `K${n}.pin1`, NC: `K${n}.pin4` }}
              pcbX={px}
              pcbY={-44}
            />
            {/* Coil-state indicator, next to the terminal it switches. */}
            <led
              name={`LED${10 + n}`}
              color="red"
              footprint="0603"
              pinLabels={LED_PINS}
              connections={{ anode: "net.V5", cathode: `R${40 + i}.pin1` }}
              pcbX={px - 4}
              pcbY={-35}
            />
            <resistor
              name={`R${40 + i}`}
              resistance="2k"
              footprint="0603"
              connections={{ pin2: `U4.${n}C` }}
              pcbX={px + 4}
              pcbY={-35}
            />
          </group>
        )
      })}
    </schematicsheet>

    {/* ============================= 5. ANALOG INPUTS AND RS-485 ========== */}
    <schematicsheet name="aio" displayName="Analog in (4-20 mA) and RS-485" sheetIndex={5}>
      {/* Loop terminal: four inputs, a common return and the 24 V rail so a
          2-wire transmitter can be powered from the board. */}
      <connector
        name="X4"
        footprint={TERM(6)}
        pinLabels={{ pin1: "AI1", pin2: "AI2", pin3: "AI3", pin4: "AI4", pin5: "AGND", pin6: "V24_AUX" }}
        connections={{ AGND: "net.GND", V24_AUX: "net.V24" }}
        pcbX={30}
        pcbY={40}
      />
      <ADS1115IDGSR
        name="U5"
        connections={{ VDD: "net.V3_3", GND: "net.GND", ADDR: "net.GND", SDA: "net.SDA", SCL: "net.SCL" }}
        pcbX={24}
        pcbY={24}
      />
      <capacitor name="C20" capacitance="100nF" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.GND" }} pcbX={24} pcbY={18} />
      <resistor name="R50" resistance="4.7k" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.SDA" }} pcbX={17} pcbY={25} />
      <resistor name="R51" resistance="4.7k" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.SCL" }} pcbX={17} pcbY={21} />
      <trace from="net.SDA" to={`U3.${I2C.sda}`} />
      <trace from="net.SCL" to={`U3.${I2C.scl}`} />

      {AI_CHANNELS.map((ain, i) => {
        const n = i + 1
        // One row per loop below the terminal: shunt, series limit, filter
        // cap, left to right into the ADC.
        const py = 32 - i * 5
        return (
          <group key={n}>
            {/* 150 ohm shunt: 4-20 mA reads 0.6-3.0 V on the +-4.096 V range. */}
            <resistor
              name={`R${60 + i}`}
              resistance="150"
              footprint="1206"
              connections={{ pin1: `X4.AI${n}`, pin2: "net.GND" }}
              pcbX={36}
              pcbY={py}
            />
            {/* Series limit plus a filter cap at the ADC pin. */}
            <resistor
              name={`R${70 + i}`}
              resistance="100"
              footprint="0603"
              connections={{ pin1: `X4.AI${n}`, pin2: `U5.${ain}` }}
              pcbX={44}
              pcbY={py}
            />
            <capacitor
              name={`C${30 + i}`}
              capacitance="100nF"
              footprint="0603"
              connections={{ pin1: `U5.${ain}`, pin2: "net.GND" }}
              pcbX={51}
              pcbY={py}
            />
          </group>
        )
      })}

      {/* RS-485 half duplex: DE and /RE tied, so one GPIO turns the driver. */}
      <SP3485EN_L_TR
        name="U6"
        connections={{
          VCC: "net.V3_3",
          GND: "net.GND",
          RO: "net.RS485_RX",
          DI: "net.RS485_TX",
          DE: "net.RS485_DE",
          N_RE: "net.RS485_DE",
          A: "net.RS485_A",
          B: "net.RS485_B",
        }}
        pcbX={62}
        pcbY={24}
      />
      <capacitor name="C21" capacitance="100nF" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.GND" }} pcbX={62} pcbY={17} />
      <trace from="net.RS485_RX" to={`U3.${RS485.rx}`} />
      <trace from="net.RS485_TX" to={`U3.${RS485.tx}`} />
      <trace from="net.RS485_DE" to={`U3.${RS485.de}`} />
      {/* Idle bias keeps the pair in a defined state with no driver on. */}
      <resistor name="R52" resistance="680" footprint="0603" connections={{ pin1: "net.V3_3", pin2: "net.RS485_A" }} pcbX={71} pcbY={26} />
      <resistor name="R53" resistance="680" footprint="0603" connections={{ pin1: "net.RS485_B", pin2: "net.GND" }} pcbX={71} pcbY={22} />
      {/* Termination fitted only on the last node of the segment. */}
      <solderjumper
        name="JP1"
        footprint="solderjumper2_p1mm_pw0.8mm_ph1mm"
        bridgedPins={[]}
        connections={{ pin1: "net.RS485_A", pin2: "R54.pin1" }}
        pcbX={71}
        pcbY={17}
        pcbRotation={180}
      />
      <resistor name="R54" resistance="120" footprint="0805" connections={{ pin2: "net.RS485_B" }} pcbX={71} pcbY={12} pcbRotation={180} />
      <connector
        name="X5"
        footprint={TERM(3)}
        pinLabels={{ pin1: "A", pin2: "B", pin3: "SHLD" }}
        connections={{ A: "net.RS485_A", B: "net.RS485_B", SHLD: "net.GND" }}
        pcbX={62}
        pcbY={40}
      />
    </schematicsheet>

    {/* Mounting: M3 in each corner, clear of every terminal courtyard. */}
    <hole name="H1" diameter="3.2mm" pcbX={-76} pcbY={47} />
    <hole name="H2" diameter="3.2mm" pcbX={76} pcbY={47} />
    <hole name="H3" diameter="3.2mm" pcbX={-76} pcbY={-47} />
    <hole name="H4" diameter="3.2mm" pcbX={76} pcbY={-47} />
  </board>
)
