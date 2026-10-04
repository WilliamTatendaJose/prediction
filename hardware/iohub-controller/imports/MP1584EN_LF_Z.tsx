import type { ChipProps } from "@tscircuit/props"

const pinLabels = {
  pin1: ["SW"],
  pin2: ["EN"],
  pin3: ["COMP"],
  pin4: ["FB"],
  pin5: ["GND"],
  pin6: ["FREQ"],
  pin7: ["VIN"],
  pin8: ["BST"],
  pin9: ["EP"]
} as const

const pinAttributes = {
  pin5: {requiresGround: true},
  pin7: {requiresPower: true}
} as const

const footprinterPinLabels = {
  ...pinLabels,
  "pin9": [...pinLabels["pin9"], "thermalpad"],
} as const

export const MP1584EN_LF_Z = (props: ChipProps<typeof pinLabels>) => {
  return (
    <chip
      pinLabels={footprinterPinLabels}
      pinAttributes={pinAttributes}
      supplierPartNumbers={{
  "jlcpcb": [
    "C15051"
  ]
}}
      manufacturerPartNumber="MP1584EN-LF-Z"
      footprint="dfn8_thermalpad2mmx2mm_thermalvias2x2_thermalviapitch1mm_thermalviaid0.3048mm_thermalviaod0.6096mm_pillpads_w7.58mm_pw0.57mm_pl2.04mm_pin1location(leftside,bottom)"
      cadModel={{
        objUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C15051.obj?uuid=d3dfb165ce8644e793b750cd6f375e75",
        stepUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C15051.step?uuid=d3dfb165ce8644e793b750cd6f375e75",
        pcbRotationOffset: 0,
        modelOriginPosition: { x: 0.000012700000070253736, y: 0, z: 0 },
      }}
      {...props}
    />
  )
}