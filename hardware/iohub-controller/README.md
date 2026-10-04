# IoHub-C1 — ESP32-S3 industrial controller

A generic field controller for the IoT Hub in [`../../iot-hub`](../../iot-hub).
It reports over WiFi (MQTT) or RS-485 (Modbus RTU — the connector the hub
already speaks), so one board covers both the greenfield and the retrofit case.

The whole design is one file, [`index.circuit.tsx`](./index.circuit.tsx),
written in [tscircuit](https://tscircuit.com): schematic, PCB and BOM all come
out of the same TSX.

## What it does

| | |
| --- | --- |
| Supply | 9–36 V DC (nominal 24 V), reverse-polarity and transient protected |
| MCU | ESP32-S3-WROOM-1-N16R8 — WiFi/BLE, 16 MB flash, 8 MB PSRAM |
| DI | 8 × 24 V opto-isolated digital inputs, sinking |
| DO | 4 × SPDT relay outputs, full changeover brought out |
| AI | 4 × 4–20 mA, 16-bit (ADS1115), 150 Ω shunts |
| Serial | RS-485 half duplex, link-selectable 120 Ω termination |
| Console | USB-C (native USB) and a 3-pin UART0 header |
| Board | 160 × 100 mm, 2 layers, M3 in each corner |

The channel counts are arrays at the top of the file. Change `DI_PINS`,
`RLY_PINS` or `AI_CHANNELS` and the schematic, the PCB and the BOM follow.

### Field wiring

| Terminal | Ways | |
| --- | --- | --- |
| X1 | 3 | 24 V in, 0 V, earth (left edge) |
| X2, X3 | 5 each | DI1–4 and DI5–8, each with its own FGND return |
| X4 | 6 | AI1–4, AGND, and 24 V out to power 2-wire transmitters |
| X5 | 3 | RS-485 A, B, shield |
| X11–X14 | 3 each | relay 1–4 changeover: COM, NO, NC |

The input side is genuinely isolated: FGND reaches only the DI terminals, the
reverse clamps and the opto LEDs' cathodes, and never touches logic GND. The
netlist check is what proves that, not the layout.

### Pin budget

Deliberately left alone: IO35/36/37 (the octal PSRAM bus on an `-R8` module),
IO0/IO3/IO45/IO46 (strapping), IO19/IO20 (native USB), and TXD0/RXD0 (the
console header).

## Working on it

```sh
npm install
npx tsci dev                                   # live preview in a browser
npx tsci build index.circuit.tsx --pcb-png --schematic-svgs --3d-png
npx tsci check netlist index.circuit.tsx
npx tsci check shorts index.circuit.tsx
npx tsci check placement index.circuit.tsx
npx tsci check schematic-placement index.circuit.tsx
npx tsci export -f gerbers index.circuit.tsx
```

Outputs land in `dist/` (gitignored).

## Where it stands

`tsci build` exits 0. The netlist check reports 0 errors and 0 warnings, the
shorts check finds none, the PCB placement check reports no issues and its DRC
is clean, and the autorouter routes all 248 traces with no errors.

Two things are deliberate rather than overlooked:

- **U1's thermal vias.** The MP1584's stock footprint drops a 2×2 via field
  inside the thermal pad, which the placement DRC reads as four vias punched
  through an SMD pad. The footprint string is overridden to keep the pad and
  drop the stitching vias; at the ~0.3 W this rail dissipates the top-layer
  pour under the pad carries it. Put the vias back — and waive the check — if
  the load grows.
- **Indicator polarity.** The 0603 LED the parts engine picks (JLCPCB C965799)
  has its *cathode* on pin 1, so `LED_PINS` relabels the part once and every
  LED is wired by `anode`/`cathode` rather than by pin number.

`tsci check schematic-placement` still reports about fifteen cosmetic issues —
net-label collisions, diode/resistor pairs not drawn collinear, a pin-padding
complaint on the built-in USB-C symbol. These are not authoring errors and
there is currently no fix from this side: `<schematicsheet>` lays its own
children out, so `schX`/`schY` on anything inside a sheet is ignored, and the
one knob the checker suggests (`schAutoLayoutEnabled`) changes nothing here.
The nets are right; only the drawing is untidy.

## Before you fab

This has never been built. Treat electrical safety, regulatory compliance and
manufacturability as yours to verify, and in particular:

- **Pick real parts for the generics.** The screw terminals use
  `pinrowN_p5.08mm` — the right pad pattern and pitch for standard 5.08 mm
  blocks (KF301/MKDS and friends drop straight on), but not a part number.
  The same goes for D1/D2/D3 and D10–D17, L1, C1, SW1/SW2 and JP1: choose
  parts and pin them with `supplierPartNumbers`, especially D2's TVS standoff
  and clamping voltage and D1's current rating.
- **Check the relay contact rating** against the load you actually intend to
  switch, and the creepage between the relay terminals and the logic side
  against whatever standard applies to you. The 21 mm relay pitch was set by
  the courtyard, not by a clearance calculation.
- **Verify the pick-and-place rotations.** The gerber export flags X4, X5, U5,
  U6 and JP1 as having no verifiable supplier pin-1 location, so their
  placement rotations are unchecked.
- **Add copper pours.** The board routes cleanly without them, but a 24 V
  switcher next to a 16-bit ADC wants a ground plane.
