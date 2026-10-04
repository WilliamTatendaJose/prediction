import type { ChipProps } from "@tscircuit/props"

const pinLabels = {
  pin1: ["RO"],
  pin2: ["N_RE"],
  pin3: ["DE"],
  pin4: ["DI"],
  pin5: ["GND"],
  pin6: ["A"],
  pin7: ["B"],
  pin8: ["VCC"]
} as const

const pinAttributes = {
  pin5: {requiresGround: true},
  pin8: {requiresPower: true}
} as const

export const SP3485EN_L_TR = (props: ChipProps<typeof pinLabels>) => {
  return (
    <chip
      pinLabels={pinLabels}
      pinAttributes={pinAttributes}
      supplierPartNumbers={{
  "jlcpcb": [
    "C8963"
  ]
}}
      manufacturerPartNumber="SP3485EN-L/TR"
      footprint="soic8_pillpads_w7.36mm_pw0.57mm_pl1.95mm_pin1location(leftside,bottom)"
      cadModel={{
        objUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C8963.obj?uuid=7abc64c95a1a4a04a4ef38f9097c870b",
        stepUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C8963.step?uuid=7abc64c95a1a4a04a4ef38f9097c870b",
        pcbRotationOffset: 0,
        modelOriginPosition: { x: 0.000012700000070253736, y: 0, z: 0 },
      }}
      {...props}
    />
  )
}